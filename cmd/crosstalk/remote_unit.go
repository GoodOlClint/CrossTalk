package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

const (
	unitLabel   = "dev.getcrosstalk.remote"
	unitService = "crosstalk-remote.service"
)

var (
	// unitRun runs launchctl, systemctl and loginctl; tests replace it so
	// nothing reaches the host.
	unitRun          = installRun
	remoteGOOS       = runtime.GOOS
	remoteCandidates = interfaceCandidates
	remoteStartWait  = 5 * time.Second
)

const enableUsage = "usage: crosstalk remote enable [--bind ADDR] [--port N] [--alias NAME] [--yes]\n"
const disableUsage = "usage: crosstalk remote disable [--yes]\n"

const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + unitLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>%[1]s</string>
		<string>remote</string>
		<string>serve</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>%[2]s</string>
	<key>StandardErrorPath</key>
	<string>%[2]s</string>
</dict>
</plist>
`

const serviceTemplate = `[Unit]
Description=CrossTalk remote gateway
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s remote serve
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func systemdArg(s string) string {
	s = strings.NewReplacer("%", "%%", "$", "$$").Replace(s)
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// unitContent is the login unit for the gateway run by bin.
func unitContent(goos, bin, home string) []byte {
	if goos == "darwin" {
		log := xmlEscape(filepath.Join(home, "Library", "Logs", "crosstalk-remote.log"))
		return fmt.Appendf(nil, plistTemplate, xmlEscape(bin), log)
	}
	return fmt.Appendf(nil, serviceTemplate, systemdArg(bin))
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// remoteBin is the path install registers when no --bin-dir is given.
func remoteBin(home string) string {
	if exe, err := installExecutable(); err == nil {
		if bin, linked, ok := installHomebrew(exe); ok && linked {
			return bin
		}
	}
	return filepath.Join(home, ".local", "bin", "crosstalk")
}

// checkUnitBin refuses a binary the unit could not start.
func checkUnitBin(ctx context.Context, bin string) error {
	fi, err := os.Stat(bin)
	if err != nil {
		return fmt.Errorf("%s: %v; run crosstalk install first", bin, err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", bin)
	}
	out, err := installRun(ctx, bin, "version")
	if err != nil {
		return fmt.Errorf("%s does not answer crosstalk version: %v", bin, err)
	}
	if v := strings.TrimSpace(string(out)); v != mcpserver.Version {
		return fmt.Errorf("%s answers crosstalk version %q, want %s", bin, v, mcpserver.Version)
	}
	return nil
}

// unitOwnership reads the unit file: present reports that it exists, owned
// that its hash is the one remote.json recorded.
func unitOwnership(home, dir string) (path string, content []byte, present, owned bool, err error) {
	path = remoteUnitPath(home)
	content, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil, false, false, nil
	}
	if err != nil {
		return path, nil, false, false, err
	}
	cfg, err := remote.LoadConfig(dir)
	if err != nil && !errors.Is(err, remote.ErrNotConfigured) {
		return path, content, true, false, err
	}
	return path, content, true, cfg.UnitSHA256 != "" && cfg.UnitSHA256 == sha256Hex(content), nil
}

func writeUnitFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	return err
}

func setUnitHash(dir, sum string) error {
	_, err := remote.UpdateConfig(dir, func(old remote.Config, configured bool) (remote.Config, error) {
		if !configured {
			return old, remote.ErrNotConfigured
		}
		old.UnitSHA256 = sum
		return old, nil
	})
	return err
}

func unitDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func unitStop(ctx context.Context) error {
	if remoteGOOS == "darwin" {
		_, err := unitRun(ctx, "launchctl", "bootout", unitDomain()+"/"+unitLabel)
		return err
	}
	_, err := unitRun(ctx, "systemctl", "--user", "disable", "--now", unitService)
	return err
}

func unitStart(ctx context.Context, path string, replace bool) error {
	if remoteGOOS == "darwin" {
		if replace {
			unitStop(ctx)
		}
		_, err := unitRun(ctx, "launchctl", "bootstrap", unitDomain(), path)
		return err
	}
	if _, err := unitRun(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if _, err := unitRun(ctx, "systemctl", "--user", "enable", "--now", unitService); err != nil {
		return err
	}
	if replace {
		_, err := unitRun(ctx, "systemctl", "--user", "restart", unitService)
		return err
	}
	return nil
}

func unitUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

// lingerOff reports Linger=no for the user on Linux; it only reads.
func lingerOff(ctx context.Context) bool {
	if remoteGOOS != "linux" {
		return false
	}
	out, err := unitRun(ctx, "loginctl", "show-user", unitUser(), "-p", "Linger")
	return err == nil && strings.TrimSpace(string(out)) == "Linger=no"
}

func lingerHint() string { return "run: loginctl enable-linger " + unitUser() }

// unitInstallApply writes and starts the unit for bin. Nothing is written
// when the file on disk is not one CrossTalk wrote.
func unitInstallApply(ctx context.Context, home, dir, bin string, stdout io.Writer) error {
	if err := checkUnitBin(ctx, bin); err != nil {
		return err
	}
	path, cur, present, owned, err := unitOwnership(home, dir)
	if err != nil {
		return err
	}
	if present && !owned {
		return fmt.Errorf("%s exists and CrossTalk did not write it (its hash is not the one in remote.json); nothing was changed. Move it away or edit it yourself", path)
	}
	want := unitContent(remoteGOOS, bin, home)
	if present && bytes.Equal(cur, want) {
		fmt.Fprintf(stdout, "    %s is already installed and identical; nothing written\n", path)
		return nil
	}
	if err := writeUnitFile(path, want); err != nil {
		return err
	}
	if err := setUnitHash(dir, sha256Hex(want)); err != nil {
		return err
	}
	if err := unitStart(ctx, path, present); err != nil {
		return fmt.Errorf("the unit was written but did not start: %w", err)
	}
	fmt.Fprintf(stdout, "    wrote and started %s\n", path)
	unitWaitAnswering(ctx, dir, stdout)
	if lingerOff(ctx) {
		fmt.Fprintf(stdout, "    Linger is off, so the gateway starts at login and stops at logout.\n    %s\n", lingerHint())
	}
	return nil
}

func unitWaitAnswering(ctx context.Context, dir string, stdout io.Writer) {
	cfg, err := remote.LoadConfig(dir)
	if err != nil {
		return
	}
	host, err := remote.LoadHost(dir)
	if err != nil {
		return
	}
	state, err := registry.DefaultDir()
	if err != nil {
		return
	}
	deadline := time.Now().Add(remoteStartWait)
	for {
		if st, err := remote.GatewayStatus(state, cfg, host.Fingerprint); err == nil && st == remote.StateRunning {
			fmt.Fprintf(stdout, "    gateway answering on %s:%d\n", cfg.Bind, cfg.Port)
			return
		}
		if !time.Now().Before(deadline) {
			fmt.Fprintf(stdout, "    gateway not answering on %s:%d yet; check: crosstalk remote status\n", cfg.Bind, cfg.Port)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// unitRemoveApply stops and deletes the unit CrossTalk wrote and clears its
// hash. It leaves any other file in place.
func unitRemoveApply(ctx context.Context, home, dir string, stdout io.Writer) error {
	path, _, present, owned, err := unitOwnership(home, dir)
	if err != nil {
		return err
	}
	if !present {
		fmt.Fprintln(stdout, "no CrossTalk login unit")
		return nil
	}
	if !owned {
		return fmt.Errorf("%s is not the unit CrossTalk wrote (it was edited or is not CrossTalk's); left in place", path)
	}
	if err := unitStop(ctx); err != nil {
		fmt.Fprintf(stdout, "    stop: %v\n", err)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if remoteGOOS != "darwin" {
		unitRun(ctx, "systemctl", "--user", "daemon-reload")
	}
	if err := setUnitHash(dir, ""); err != nil && !errors.Is(err, remote.ErrNotConfigured) {
		return err
	}
	fmt.Fprintf(stdout, "removed %s\n", path)
	return nil
}

// remoteEnableSteps are the steps of remote enable: write the config when
// flags were given, then the login unit. A unit that CrossTalk did not write
// refuses the whole plan before any step runs.
func remoteEnableSteps(home, dir, bin string, f cfgFlags, stdout io.Writer) ([]harness.Step, error) {
	old, err := remote.LoadConfig(dir)
	configured := err == nil
	if err != nil && !errors.Is(err, remote.ErrNotConfigured) {
		return nil, err
	}
	if len(f.set) > 0 || !configured {
		if _, err := f.apply(old, configured); err != nil {
			return nil, err
		}
	}
	path, _, present, owned, err := unitOwnership(home, dir)
	if err != nil {
		return nil, err
	}
	if present && !owned {
		return nil, fmt.Errorf("%s exists and CrossTalk did not write it (its hash is not the one in remote.json); nothing was changed. Move it away or edit it yourself", path)
	}
	var steps []harness.Step
	if len(f.set) > 0 {
		next, _ := f.apply(old, configured)
		rePair, err := rePairAliases(dir, old, next, configured)
		if err != nil {
			return nil, err
		}
		change := fmt.Sprintf("write %s\nalias %s, bind %s, port %d\ncreate %s and %s if missing", filepath.Join(dir, "remote.json"), next.Alias, next.Bind, next.Port, filepath.Join(dir, "host.key"), filepath.Join(dir, "host.crt"))
		steps = append(steps, harness.Step{
			Title:  "remote config",
			Change: change,
			Apply:  func(context.Context) error { return f.write(dir, rePair, stdout) },
		})
	}
	steps = append(steps, harness.Step{
		Title:  "login unit",
		Change: fmt.Sprintf("write %s (mode 0644) and start it\n%s", path, bytes.TrimRight(unitContent(remoteGOOS, bin, home), "\n")),
		Apply:  func(ctx context.Context) error { return unitInstallApply(ctx, home, dir, bin, stdout) },
	})
	return steps, nil
}

func parseEnableFlags(name string, args []string, withBind bool) (f cfgFlags, yes bool, err error) {
	fs := flag.NewFlagSet("remote "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&yes, "yes", false, "")
	var binds []string
	if withBind {
		fs.Func("bind", "", func(s string) error { binds = append(binds, s); return nil })
		fs.IntVar(&f.port, "port", 0, "")
		fs.StringVar(&f.alias, "alias", "", "")
	}
	if err = fs.Parse(args); err == nil && fs.NArg() > 0 {
		err = fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err == nil && len(binds) > 1 {
		err = errors.New("--bind takes one address: the config holds a single bind address")
	}
	if len(binds) == 1 {
		f.bind = binds[0]
	}
	f.set = map[string]bool{}
	fs.Visit(func(x *flag.Flag) { f.set[x.Name] = true })
	delete(f.set, "yes")
	return f, yes, err
}

// applySteps shows each step and applies it, asking first unless yes.
func applySteps(ctx context.Context, steps []harness.Step, yes bool, answers *bufio.Reader, stdout, stderr io.Writer) bool {
	for _, s := range steps {
		fmt.Fprintf(stdout, "- %s\n%s\n", s.Title, installIndent(s.Change))
		if !yes && !installConfirm(answers, stdout) {
			fmt.Fprintln(stderr, "stopped: step declined")
			return false
		}
		if err := s.Apply(ctx); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", s.Title, err)
			return false
		}
	}
	return true
}

func runRemoteEnable(args []string, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	f, yes, err := parseEnableFlags("enable", args, true)
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk remote enable: %v\n%s", err, enableUsage)
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk remote enable: %v\n", err)
		return 1
	}
	dir, err := remote.ConfigDir()
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk remote enable: %v\n", err)
		return 1
	}
	bin := remoteBin(home)
	steps, err := remoteEnableSteps(home, dir, bin, f, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk remote enable: %v\n", err)
		if errors.As(err, new(errUsage)) {
			return 2
		}
		return 1
	}
	ctx := context.Background()
	if err := checkUnitBin(ctx, bin); err != nil {
		fmt.Fprintf(stderr, "crosstalk remote enable: %v\n", err)
		return 1
	}
	if !yes && !tty {
		for _, s := range steps {
			fmt.Fprintf(stdout, "- %s\n%s\n", s.Title, installIndent(s.Change))
		}
		fmt.Fprintln(stderr, "crosstalk remote enable: no terminal to confirm on; nothing changed. Run it in a terminal or pass --yes.")
		return 2
	}
	if !applySteps(ctx, steps, yes, bufio.NewReader(in), stdout, stderr) {
		return 1
	}
	return 0
}

func runRemoteDisable(args []string, in io.Reader, tty bool, stdout, stderr io.Writer) int {
	_, yes, err := parseEnableFlags("disable", args, false)
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk remote disable: %v\n%s", err, disableUsage)
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk remote disable: %v\n", err)
		return 1
	}
	dir, err := remote.ConfigDir()
	if err != nil {
		fmt.Fprintf(stderr, "crosstalk remote disable: %v\n", err)
		return 1
	}
	path, _, present, owned, err := unitOwnership(home, dir)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "crosstalk remote disable: %v\n", err)
		return 1
	case !present:
		fmt.Fprintln(stdout, "no CrossTalk login unit")
		return 0
	case !owned:
		fmt.Fprintf(stderr, "crosstalk remote disable: %s is not the unit CrossTalk wrote (it was edited or is not CrossTalk's); left in place\n", path)
		return 1
	}
	step := harness.Step{Title: "login unit", Change: "stop and remove " + path}
	fmt.Fprintf(stdout, "- %s\n%s\n", step.Title, installIndent(step.Change))
	if !yes {
		if !tty {
			fmt.Fprintln(stderr, "crosstalk remote disable: no terminal to confirm on; nothing changed. Run it in a terminal or pass --yes.")
			return 2
		}
		if !installConfirm(bufio.NewReader(in), stdout) {
			fmt.Fprintln(stderr, "stopped: declined")
			return 1
		}
	}
	if err := unitRemoveApply(context.Background(), home, dir, stdout); err != nil {
		fmt.Fprintf(stderr, "crosstalk remote disable: %v\n", err)
		return 1
	}
	return 0
}

// unitUninstallStep removes the unit when no harness still runs the binary.
// keep reports why the binary stays, or "".
func unitUninstallStep(home, dir string, keep func() string, w io.Writer) (harness.Step, bool) {
	path, _, present, _, _ := unitOwnership(home, dir)
	if !present {
		return harness.Step{}, false
	}
	return harness.Step{
		Title:  "login unit",
		Change: "stop and remove " + path + " unless another harness still runs the binary or the file was edited",
		Apply: func(ctx context.Context) error {
			if why := keep(); why != "" {
				fmt.Fprintf(w, "    kept %s: %s\n", path, why)
				return nil
			}
			_, _, _, owned, err := unitOwnership(home, dir)
			if err != nil {
				return err
			}
			if !owned {
				fmt.Fprintf(w, "    left %s: it was edited or is not CrossTalk's, so it is not removed; it still runs a binary being uninstalled, remove it yourself\n", path)
				return nil
			}
			return unitRemoveApply(ctx, home, dir, w)
		},
	}, true
}

// orderCandidates keeps tailnet (100.64.0.0/10) and private LAN IPv4
// addresses, tailnet first, each group ascending.
func orderCandidates(addrs []string) []string {
	tailnet := netip.MustParsePrefix("100.64.0.0/10")
	var tail, lan []netip.Addr
	for _, s := range addrs {
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is4() {
			continue
		}
		switch {
		case tailnet.Contains(a):
			tail = append(tail, a)
		case a.IsPrivate():
			lan = append(lan, a)
		}
	}
	var out []string
	for _, g := range [][]netip.Addr{tail, lan} {
		slices.SortFunc(g, netip.Addr.Compare)
		for _, a := range slices.Compact(g) {
			out = append(out, a.String())
		}
	}
	return out
}

func interfaceCandidates() []string {
	ias, err := remote.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var all []string
	for _, ia := range ias {
		if p, err := netip.ParsePrefix(ia.String()); err == nil {
			all = append(all, p.Addr().Unmap().String())
		}
	}
	return orderCandidates(all)
}

// askRemoteBind lists the candidate addresses and returns the one chosen.
func askRemoteBind(answers *bufio.Reader, stdout io.Writer) (string, bool) {
	cands := remoteCandidates()
	if len(cands) == 0 {
		fmt.Fprintln(stdout, "No tailnet or private LAN address found on this host; remote setup skipped. Later: crosstalk remote enable --bind ADDR")
		return "", false
	}
	for i, c := range cands {
		fmt.Fprintf(stdout, "  %d) %s\n", i+1, c)
	}
	fmt.Fprint(stdout, "Bind to which address (number or address)? ")
	line, _ := answers.ReadString('\n')
	a := strings.TrimSpace(line)
	if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(cands) {
		return cands[n-1], true
	}
	if slices.Contains(cands, a) {
		return a, true
	}
	fmt.Fprintln(stdout, "No address chosen; remote setup skipped. Later: crosstalk remote enable --bind ADDR")
	return "", false
}

const remoteNext = "next: crosstalk remote pair on this host, crosstalk remote join on the other"

func gatewayRunningMsg(stateDir string) string {
	return fmt.Sprintf("a gateway is running (pid %s); stop it, then run uninstall --purge again", remote.GatewayHolderPID(stateDir))
}

// installRemoteGroup is install's remote step group: with --remote the steps
// planned from --bind, on a terminal without --yes and --remote the question
// (default no) and then the address. Without either it writes nothing.
func installRemoteGroup(ctx context.Context, o installOpts, home, cfgDir, bin string, steps []harness.Step, tty bool, answers *bufio.Reader, stdout, stderr io.Writer) bool {
	if o.mode == "uninstall" || o.check {
		return true
	}
	f := o.cfgFlags()
	if len(steps) == 0 {
		switch {
		case o.remote && !o.yes:
		case tty && !o.yes && !o.remote:
			fmt.Fprint(stdout, "Set up remote messaging to your other machines? This starts a network listener at login. [y/N] ")
			line, _ := answers.ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				return true
			}
		default:
			return true
		}
		bind, ok := askRemoteBind(answers, stdout)
		if !ok {
			return true
		}
		f.bind = bind
		f.set["bind"] = true
		var err error
		if steps, err = remoteEnableSteps(home, cfgDir, bin, f, stdout); err != nil {
			fmt.Fprintf(stderr, "remote: %v\n", err)
			return false
		}
	}
	fmt.Fprintln(stdout, "== remote")
	if !applySteps(ctx, steps, o.yes, answers, stdout, stderr) {
		return false
	}
	fmt.Fprintln(stdout, remoteNext)
	return true
}
