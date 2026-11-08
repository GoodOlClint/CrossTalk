package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/GoodOlClint/crosstalk/internal/remote"
)

func init() {
	unitRun = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("test did not install a command runner; refusing to run a command on the host")
	}
}

type runLog struct {
	mu     sync.Mutex
	calls  []string
	linger string
	hook   func(call string)
	failOn string
}

func (r *runLog) run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := name + " " + strings.Join(args, " ")
	r.mu.Lock()
	r.calls = append(r.calls, call)
	hook := r.hook
	failOn := r.failOn
	r.mu.Unlock()
	if failOn != "" && strings.Contains(call, failOn) {
		return nil, errors.New("forced failure")
	}
	if hook != nil {
		hook(call)
	}
	if name == "loginctl" && len(args) > 0 && args[0] == "show-user" {
		return []byte("Linger=" + r.linger + "\n"), nil
	}
	return nil, nil
}

func (r *runLog) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *runLog) count(sub string) int {
	n := 0
	for _, c := range r.all() {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// fakeGateway holds gateway.lock and answers TLS handshakes with the host
// certificate on bind:port, which is all status and K8 look at.
type fakeGateway struct {
	lock *os.File
	ln   net.Listener
}

func startFakeGateway(t *testing.T, home string) *fakeGateway {
	t.Helper()
	dir := filepath.Join(home, ".config", "crosstalk")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	cfg, err := remote.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	host, err := remote.LoadHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGateway{}
	g.lock = holdGatewayLock(t, state)
	g.ln, err = tls.Listen("tcp", net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port)), &tls.Config{Certificates: []tls.Certificate{host.Certificate}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := g.ln.Accept()
			if err != nil {
				return
			}
			go func() {
				c.(*tls.Conn).Handshake()
				c.Close()
			}()
		}
	}()
	t.Cleanup(g.stop)
	return g
}

func (g *fakeGateway) stop() {
	if g.ln != nil {
		g.ln.Close()
		g.ln = nil
	}
	if g.lock != nil {
		g.lock.Close()
		g.lock = nil
	}
}

func holdGatewayLock(t *testing.T, state string) *os.File {
	t.Helper()
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(state, "gateway.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	f.Truncate(0)
	f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	t.Cleanup(func() { f.Close() })
	return f
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// unitSandbox is installSandbox plus a command runner that records every
// launchctl, systemctl and loginctl call instead of running it, and a fake
// gateway that comes up when the unit is started and goes down when stopped.
func unitSandbox(t *testing.T, goos string, clis ...string) (home string, rec *runLog) {
	t.Helper()
	home = installSandbox(t, clis...)
	rec = &runLog{linger: "yes"}
	oldRun, oldOS, oldCand := unitRun, remoteGOOS, remoteCandidates
	unitRun, remoteGOOS = rec.run, goos
	remoteCandidates = func() []string { return []string{"127.0.0.1"} }
	t.Cleanup(func() { unitRun, remoteGOOS, remoteCandidates = oldRun, oldOS, oldCand })
	var gw *fakeGateway
	rec.hook = func(call string) {
		switch {
		case strings.Contains(call, "launchctl bootstrap"), strings.Contains(call, "enable --now"):
			if gw == nil {
				gw = startFakeGateway(t, home)
			}
		case strings.Contains(call, "systemctl --user restart"):
			if gw != nil {
				gw.stop()
			}
			gw = startFakeGateway(t, home)
		case strings.Contains(call, "launchctl bootout"), strings.Contains(call, "disable --now"):
			if gw != nil {
				gw.stop()
				gw = nil
			}
		}
	}
	src, err := installExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := installBinary(src, filepath.Join(home, ".local", "bin", "crosstalk")); err != nil {
		t.Fatal(err)
	}
	return home, rec
}

func enableCmd(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := runRemote(append([]string{"enable"}, args...), strings.NewReader(""), false, &out, &out)
	return code, out.String()
}

func disableCmd(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := runRemote(append([]string{"disable"}, args...), strings.NewReader(""), false, &out, &out)
	return code, out.String()
}

func eachOS(t *testing.T, f func(t *testing.T, goos string)) {
	t.Helper()
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) { f(t, goos) })
	}
}

// hostOS runs f for the OS the tests run on; the install-driven tests are
// slow and their unit commands are the only OS-dependent part.
func hostOS(t *testing.T, f func(t *testing.T, goos string)) {
	t.Helper()
	goos := runtime.GOOS
	if goos != "darwin" {
		goos = "linux"
	}
	t.Run(goos, func(t *testing.T) { f(t, goos) })
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func mustEnable(t *testing.T, port int) {
	t.Helper()
	if code, out := enableCmd(t, "--bind", "127.0.0.1", "--port", strconv.Itoa(port), "--yes"); code != 0 {
		t.Fatalf("enable: exit %d\n%s", code, out)
	}
}

func installRemote(t *testing.T, bin string, extra ...string) (int, string) {
	t.Helper()
	args := append([]string{"--yes", "--remote", "--bind", "127.0.0.1", "--port", strconv.Itoa(freePort(t)), "--bin-dir", bin}, extra...)
	return installRunCmd(t, "install", args...)
}

func unitAbsent(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Lstat(remoteUnitPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("login unit %s exists: %v", remoteUnitPath(home), err)
	}
}

func TestEnableIdempotent(t *testing.T) {
	eachOS(t, func(t *testing.T, goos string) {
		home, rec := unitSandbox(t, goos)
		port := freePort(t)
		mustEnable(t, port)
		unit := remoteUnitPath(home)
		bin := filepath.Join(home, ".local", "bin", "crosstalk")
		first, err := os.ReadFile(unit)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{bin}
		if goos == "darwin" {
			want = append(want, "dev.getcrosstalk.remote", "<key>RunAtLoad</key>\n\t<true/>", "<key>SuccessfulExit</key>\n\t\t<false/>", filepath.Join(home, "Library", "Logs", "crosstalk-remote.log"))
		} else {
			want = append(want, "ExecStart="+bin+" remote serve", "Restart=on-failure", "WantedBy=default.target")
		}
		for _, w := range want {
			if !strings.Contains(string(first), w) {
				t.Errorf("unit lacks %q:\n%s", w, first)
			}
		}
		dir := filepath.Join(home, ".config", "crosstalk")
		cfg, err := remote.LoadConfig(dir)
		if err != nil || cfg.UnitSHA256 != fileSHA(t, unit) {
			t.Errorf("unit_sha256 = %q, %v; want %s", cfg.UnitSHA256, err, fileSHA(t, unit))
		}
		before := installSnapshot(t, home)
		if code, out := enableCmd(t, "--yes"); code != 0 {
			t.Fatalf("second enable: exit %d\n%s", code, out)
		}
		second, _ := os.ReadFile(unit)
		if !bytes.Equal(first, second) {
			t.Errorf("unit changed on the second enable:\n%s\n---\n%s", first, second)
		}
		if after := installSnapshot(t, home); fmt.Sprint(before) != fmt.Sprint(after) {
			t.Errorf("second enable changed HOME:\nbefore %v\nafter  %v", before, after)
		}
		starts := rec.count("launchctl bootstrap") + rec.count("enable --now")
		if starts != 1 {
			t.Errorf("start commands = %d over two enables, want 1: %v", starts, rec.all())
		}
	})
}

func TestEnableRefusesForeignUnit(t *testing.T) {
	eachOS(t, func(t *testing.T, goos string) {
		t.Run("foreign", func(t *testing.T) {
			home, rec := unitSandbox(t, goos)
			installWrite(t, remoteUnitPath(home), "someone else's unit\n")
			before := installSnapshot(t, home)
			code, out := enableCmd(t, "--bind", "127.0.0.1", "--yes")
			if code != 1 || !strings.Contains(out, remoteUnitPath(home)) {
				t.Errorf("enable over a foreign unit: exit %d, want 1 naming the path\n%s", code, out)
			}
			if after := installSnapshot(t, home); fmt.Sprint(before) != fmt.Sprint(after) {
				t.Errorf("HOME changed:\nbefore %v\nafter  %v", before, after)
			}
			if c := rec.all(); len(c) != 0 {
				t.Errorf("commands ran: %v", c)
			}
		})
		t.Run("edited", func(t *testing.T) {
			home, rec := unitSandbox(t, goos)
			mustEnable(t, freePort(t))
			unit := remoteUnitPath(home)
			b, _ := os.ReadFile(unit)
			installWrite(t, unit, string(b)+"\n# edited by hand\n")
			before, ran := installSnapshot(t, home), len(rec.all())
			code, out := enableCmd(t, "--yes")
			if code != 1 || !strings.Contains(out, unit) {
				t.Errorf("enable over an edited unit: exit %d, want 1 naming the path\n%s", code, out)
			}
			if after := installSnapshot(t, home); fmt.Sprint(before) != fmt.Sprint(after) {
				t.Errorf("HOME changed:\nbefore %v\nafter  %v", before, after)
			}
			if n := len(rec.all()); n != ran {
				t.Errorf("commands ran after the refusal: %v", rec.all()[ran:])
			}
		})
	})
}

func TestDisableRemovesOnlyOwnUnit(t *testing.T) {
	eachOS(t, func(t *testing.T, goos string) {
		home, rec := unitSandbox(t, goos)
		mustEnable(t, freePort(t))
		unit := remoteUnitPath(home)
		code, out := disableCmd(t, "--yes")
		if code != 0 || !strings.Contains(out, "removed "+unit) {
			t.Errorf("disable: exit %d, want 0 with removed %s\n%s", code, unit, out)
		}
		unitAbsent(t, home)
		if rec.count("launchctl bootout")+rec.count("disable --now") != 1 {
			t.Errorf("unit not stopped once: %v", rec.all())
		}
		cfg, err := remote.LoadConfig(filepath.Join(home, ".config", "crosstalk"))
		if err != nil || cfg.UnitSHA256 != "" {
			t.Errorf("config after disable = %+v, %v; want kept with unit_sha256 empty", cfg, err)
		}
		if code, out := disableCmd(t, "--yes"); code != 0 || !strings.Contains(out, "no CrossTalk login unit") {
			t.Errorf("second disable: exit %d\n%s", code, out)
		}
		mustEnable(t, freePort(t))
		b, _ := os.ReadFile(unit)
		installWrite(t, unit, string(b)+"\n# edited by hand\n")
		ran := len(rec.all())
		code, out = disableCmd(t, "--yes")
		if code != 1 || !strings.Contains(out, unit) {
			t.Errorf("disable over an edited unit: exit %d, want 1 naming the path\n%s", code, out)
		}
		if got := installRead(t, unit); !strings.Contains(got, "edited by hand") {
			t.Errorf("edited unit was removed or rewritten:\n%s", got)
		}
		if n := len(rec.all()); n != ran {
			t.Errorf("commands ran for an edited unit: %v", rec.all()[ran:])
		}
	})
}

// scripted answers prompts from what has been printed: apply? with y, the
// remote question with remoteAns and the address question with pick.
type scripted struct {
	out        *bytes.Buffer
	remoteAns  string
	pick       string
	asked, pks int
}

func (s *scripted) Read(p []byte) (int, error) {
	o := s.out.String()
	last := o[strings.LastIndex(o, "\n")+1:]
	ans := "\n"
	switch {
	case strings.HasSuffix(o, "[y/N] ") && strings.Contains(last, "remote messaging"):
		s.asked++
		ans = s.remoteAns + "\n"
	case strings.HasSuffix(o, "apply? [y/N] "):
		ans = "y\n"
	case strings.HasSuffix(o, "address)? "):
		s.pks++
		ans = s.pick + "\n"
	}
	return copy(p, ans), nil
}

func installInteractive(t *testing.T, remoteAns, pick string, args ...string) (int, string, *scripted) {
	t.Helper()
	var out bytes.Buffer
	in := &scripted{out: &out, remoteAns: remoteAns, pick: pick}
	code := runInstall("install", args, in, true, &out, &out)
	return code, out.String(), in
}

func remoteFiles(home string) []string {
	var found []string
	for _, p := range []string{
		filepath.Join(home, ".config", "crosstalk", "remote.json"),
		filepath.Join(home, ".config", "crosstalk", "host.key"),
		remoteUnitPath(home),
	} {
		if _, err := os.Lstat(p); err == nil {
			found = append(found, p)
		}
	}
	return found
}

func TestInstallAsksRemoteDefaultNo(t *testing.T) {
	hostOS(t, func(t *testing.T, goos string) {
		home, rec := unitSandbox(t, goos, "claude")
		code, out, in := installInteractive(t, "", "", "--bin-dir", filepath.Join(home, "bin"))
		if code != 0 {
			t.Fatalf("install: exit %d\n%s", code, out)
		}
		if in.asked != 1 || !strings.Contains(out, "Set up remote messaging to your other machines? This starts a network listener at login. [y/N]") {
			t.Errorf("remote question asked %d times:\n%s", in.asked, out)
		}
		if f := remoteFiles(home); len(f) != 0 {
			t.Errorf("an empty answer wrote %v", f)
		}
		if c := rec.all(); len(c) != 0 {
			t.Errorf("commands ran: %v", c)
		}
		if strings.Contains(out, "next: crosstalk remote pair") {
			t.Errorf("printed the remote next line:\n%s", out)
		}
	})
}

func TestInstallAsksRemoteYesBindsChosenAddress(t *testing.T) {
	home, rec := unitSandbox(t, "linux", "claude")
	code, out, _ := installInteractive(t, "y", "1", "--bin-dir", filepath.Join(home, "bin"))
	if code != 0 {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	cfg, err := remote.LoadConfig(filepath.Join(home, ".config", "crosstalk"))
	if err != nil || cfg.Bind != "127.0.0.1" {
		t.Errorf("config = %+v, %v; want bind 127.0.0.1", cfg, err)
	}
	if rec.count("enable --now") != 1 || !strings.Contains(out, "next: crosstalk remote pair on this host, crosstalk remote join on the other") {
		t.Errorf("remote step group did not run to its next line (calls %v):\n%s", rec.all(), out)
	}
	if strings.Contains(out, "0.0.0.0") {
		t.Errorf("a wildcard address was offered:\n%s", out)
	}
}

func TestInstallYesNeverRemote(t *testing.T) {
	hostOS(t, func(t *testing.T, goos string) {
		home, rec := unitSandbox(t, goos, "claude")
		bin := filepath.Join(home, "bin")
		var out bytes.Buffer
		if code := runInstall("install", []string{"--yes", "--bin-dir", bin}, strings.NewReader(""), true, &out, &out); code != 0 {
			t.Fatalf("install --yes on a terminal: exit %d\n%s", code, out.String())
		}
		if strings.Contains(out.String(), "remote messaging") {
			t.Errorf("--yes asked about remote:\n%s", out.String())
		}
		if f := remoteFiles(home); len(f) != 0 {
			t.Errorf("--yes without --remote wrote %v", f)
		}
		if _, err := os.Stat(filepath.Join(home, ".config", "crosstalk")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("config dir exists: %v", err)
		}
		if c := rec.all(); len(c) != 0 {
			t.Errorf("commands ran: %v", c)
		}
	})
}

func TestInstallRemoteNeedsBindWithYes(t *testing.T) {
	home, rec := unitSandbox(t, "linux", "claude")
	before := installSnapshot(t, home)
	for _, mode := range []string{"install", "reinstall"} {
		code, out := installRunCmd(t, mode, "--remote", "--yes", "--bin-dir", filepath.Join(home, "bin"))
		if code != 2 || !strings.Contains(out, "--bind") {
			t.Errorf("%s --remote --yes: exit %d, want 2 naming --bind\n%s", mode, code, out)
		}
	}
	if after := installSnapshot(t, home); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("HOME changed:\nbefore %v\nafter  %v", before, after)
	}
	if c := rec.all(); len(c) != 0 {
		t.Errorf("commands ran: %v", c)
	}
}

func TestUninstallRemovesUnit(t *testing.T) {
	hostOS(t, func(t *testing.T, goos string) {
		home, rec := unitSandbox(t, goos, "claude")
		bin := filepath.Join(home, "bin")
		if code, out := installRemote(t, bin); code != 0 {
			t.Fatalf("install --remote: exit %d\n%s", code, out)
		}
		if _, err := os.Stat(remoteUnitPath(home)); err != nil {
			t.Fatalf("install --remote wrote no unit: %v", err)
		}
		if !strings.Contains(installRead(t, remoteUnitPath(home)), filepath.Join(bin, "crosstalk")) {
			t.Errorf("unit does not name the installed binary %s:\n%s", filepath.Join(bin, "crosstalk"), installRead(t, remoteUnitPath(home)))
		}
		code, out := installRunCmd(t, "uninstall", "--yes", "--bin-dir", bin)
		if code != 0 {
			t.Fatalf("uninstall: exit %d\n%s", code, out)
		}
		unitAbsent(t, home)
		if rec.count("launchctl bootout")+rec.count("disable --now") != 1 {
			t.Errorf("unit not stopped once: %v", rec.all())
		}
		if _, err := os.Stat(filepath.Join(bin, "crosstalk")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("binary kept: %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, ".config", "crosstalk", "remote.json")); err != nil {
			t.Errorf("uninstall without --purge removed the config: %v", err)
		}
	})
}

func TestUninstallNamedLastHarnessRemovesUnit(t *testing.T) {
	hostOS(t, func(t *testing.T, goos string) {
		home, _ := unitSandbox(t, goos, "claude", "codex")
		bin := filepath.Join(home, "bin")
		if code, out := installRemote(t, bin); code != 0 {
			t.Fatalf("install --remote: exit %d\n%s", code, out)
		}
		if code, out := installRunCmd(t, "uninstall", "claude", "--yes", "--bin-dir", bin); code != 0 {
			t.Fatalf("uninstall claude: exit %d\n%s", code, out)
		}
		if _, err := os.Stat(remoteUnitPath(home)); err != nil {
			t.Fatalf("unit removed while codex still runs the binary: %v", err)
		}
		code, out := installRunCmd(t, "uninstall", "codex", "--yes", "--bin-dir", bin)
		if code != 0 {
			t.Fatalf("uninstall codex: exit %d\n%s", code, out)
		}
		unitAbsent(t, home)
	})
}

func TestUninstallKeepsUnitWhileBinaryKept(t *testing.T) {
	hostOS(t, func(t *testing.T, goos string) {
		cases := map[string]func(t *testing.T, home, bin string) (clis []string, uninstall string){
			"project registration": func(t *testing.T, home, bin string) ([]string, string) {
				proj := filepath.Join(home, "proj")
				installWrite(t, filepath.Join(home, ".claude.json"), `{"projects":{"`+proj+`":{"mcpServers":{"crosstalk":{"command":"`+filepath.Join(bin, "crosstalk")+`","args":["mcp"]}}}}}`)
				return []string{"codex"}, "codex"
			},
		}
		for name, seed := range cases {
			t.Run(name, func(t *testing.T) {
				home, rec := unitSandbox(t, goos, "claude", "codex")
				bin := filepath.Join(home, "bin")
				if code, out := installRemote(t, bin); code != 0 {
					t.Fatalf("install --remote: exit %d\n%s", code, out)
				}
				_, which := seed(t, home, bin)
				ran := len(rec.all())
				code, out := installRunCmd(t, "uninstall", which, "--yes", "--bin-dir", bin)
				if code != 0 {
					t.Fatalf("uninstall %s: exit %d\n%s", which, code, out)
				}
				if _, err := os.Stat(remoteUnitPath(home)); err != nil {
					t.Errorf("unit removed while the binary is kept: %v\n%s", err, out)
				}
				if _, err := os.Stat(filepath.Join(bin, "crosstalk")); err != nil {
					t.Errorf("binary removed: %v\n%s", err, out)
				}
				if n := len(rec.all()); n != ran {
					t.Errorf("unit commands ran while the binary is kept: %v", rec.all()[ran:])
				}
			})
		}
	})
}

func TestUninstallLeavesEditedUnit(t *testing.T) {
	hostOS(t, func(t *testing.T, goos string) {
		home, rec := unitSandbox(t, goos, "claude")
		bin := filepath.Join(home, "bin")
		if code, out := installRemote(t, bin); code != 0 {
			t.Fatalf("install --remote: exit %d\n%s", code, out)
		}
		unit := remoteUnitPath(home)
		edited := installRead(t, unit) + "\n# edited by hand\n"
		installWrite(t, unit, edited)
		ran := len(rec.all())
		code, out := installRunCmd(t, "uninstall", "--yes", "--bin-dir", bin)
		if code != 0 {
			t.Fatalf("uninstall: exit %d\n%s", code, out)
		}
		if installRead(t, unit) != edited {
			t.Errorf("edited unit was removed or rewritten")
		}
		if !strings.Contains(out, unit) {
			t.Errorf("output does not print the unit path %s:\n%s", unit, out)
		}
		if n := len(rec.all()); n != ran {
			t.Errorf("commands ran for an edited unit: %v", rec.all()[ran:])
		}
	})
}

func addPeers(t *testing.T, home string, aliases ...string) {
	t.Helper()
	ps := remote.NewPeers(filepath.Join(home, ".config", "crosstalk"))
	for i, a := range aliases {
		if err := ps.Upsert(remote.Peer{Alias: a, Address: "10.0.0." + strconv.Itoa(i+2) + ":7745", Fingerprint: strings.Repeat(strconv.Itoa(i+1), 64)}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPurgeRemovesConfigAndListsPeers(t *testing.T) {
	hostOS(t, func(t *testing.T, goos string) {
		home, _ := unitSandbox(t, goos, "claude")
		bin := filepath.Join(home, "bin")
		if code, out := installRemote(t, bin); code != 0 {
			t.Fatalf("install --remote: exit %d\n%s", code, out)
		}
		addPeers(t, home, "desk", "vm")
		code, out := installRunCmd(t, "uninstall", "--purge", "--yes", "--bin-dir", bin)
		if code != 0 {
			t.Fatalf("uninstall --purge: exit %d\n%s", code, out)
		}
		for _, p := range []string{filepath.Join(home, ".config", "crosstalk"), filepath.Join(home, ".local", "state", "crosstalk")} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s kept: %v", p, err)
			}
		}
		if !strings.Contains(out, "unpair this host on: desk, vm") {
			t.Errorf("output does not list the peers to unpair on:\n%s", out)
		}
		unitAbsent(t, home)
	})
}

func TestPurgeKeepsConfigWhileStateKept(t *testing.T) {
	hostOS(t, func(t *testing.T, goos string) {
		home, _ := unitSandbox(t, goos, "claude", "codex")
		bin := filepath.Join(home, "bin")
		if code, out := installRemote(t, bin); code != 0 {
			t.Fatalf("install --remote: exit %d\n%s", code, out)
		}
		addPeers(t, home, "desk")
		code, out := installRunCmd(t, "uninstall", "claude", "--purge", "--yes", "--bin-dir", bin)
		if code != 0 {
			t.Fatalf("uninstall claude --purge: exit %d\n%s", code, out)
		}
		for _, f := range []string{"remote.json", "host.key", "peers.json"} {
			if _, err := os.Stat(filepath.Join(home, ".config", "crosstalk", f)); err != nil {
				t.Errorf("%s removed while state is kept: %v\n%s", f, err, out)
			}
		}
		if strings.Contains(out, "unpair this host on") {
			t.Errorf("printed the unpair line although nothing was deleted:\n%s", out)
		}
	})
}

func TestUninstallHomebrewRemovesUnit(t *testing.T) {
	hostOS(t, func(t *testing.T, goos string) {
		home, rec := unitSandbox(t, goos, "claude")
		prefix := installBrewKeg(t, "opt/homebrew", true)
		link := filepath.Join(prefix, "bin", "crosstalk")
		if code, out := installRunCmd(t, "install", "--yes", "--remote", "--bind", "127.0.0.1", "--port", strconv.Itoa(freePort(t))); code != 0 {
			t.Fatalf("install --remote: exit %d\n%s", code, out)
		}
		if !strings.Contains(installRead(t, remoteUnitPath(home)), link) {
			t.Fatalf("unit does not name the Homebrew binary %s:\n%s", link, installRead(t, remoteUnitPath(home)))
		}
		code, out := installRunCmd(t, "uninstall", "--yes")
		if code != 0 {
			t.Fatalf("uninstall: exit %d\n%s", code, out)
		}
		if _, err := os.Stat(link); err != nil {
			t.Errorf("Homebrew binary removed: %v", err)
		}
		unitAbsent(t, home)
		if rec.count("launchctl bootout")+rec.count("disable --now") != 1 {
			t.Errorf("unit not stopped once: %v", rec.all())
		}
	})
}

func TestPurgeRefusesRunningGateway(t *testing.T) {
	hostOS(t, func(t *testing.T, goos string) {
		for _, edited := range []bool{false, true} {
			name := map[bool]string{false: "foreground", true: "edited unit"}[edited]
			t.Run(name, func(t *testing.T) {
				home, _ := unitSandbox(t, goos, "claude")
				bin := filepath.Join(home, "bin")
				if edited {
					if code, out := installRemote(t, bin); code != 0 {
						t.Fatalf("install --remote: exit %d\n%s", code, out)
					}
					installWrite(t, remoteUnitPath(home), installRead(t, remoteUnitPath(home))+"\n# edited by hand\n")
				} else {
					if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
						t.Fatalf("install: exit %d\n%s", code, out)
					}
					holdGatewayLock(t, filepath.Join(home, ".local", "state", "crosstalk"))
				}
				before := installSnapshot(t, home)
				code, out := installRunCmd(t, "uninstall", "--purge", "--yes", "--bin-dir", bin)
				want := fmt.Sprintf("a gateway is running (pid %d); stop it, then run uninstall --purge again", os.Getpid())
				if code != 1 || !strings.Contains(out, want) {
					t.Errorf("uninstall --purge: exit %d, want 1 with %q\n%s", code, want, out)
				}
				if after := installSnapshot(t, home); fmt.Sprint(before) != fmt.Sprint(after) {
					t.Errorf("HOME changed:\nbefore %v\nafter  %v", before, after)
				}
			})
		}
	})
}

func TestLingerHintPrintedNeverRun(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	hint := "run: loginctl enable-linger " + u.Username
	t.Run("enable", func(t *testing.T) {
		home, rec := unitSandbox(t, "linux")
		rec.linger = "no"
		code, out := enableCmd(t, "--bind", "127.0.0.1", "--port", strconv.Itoa(freePort(t)), "--yes")
		if code != 0 || !strings.Contains(out, hint) {
			t.Errorf("enable: exit %d, want 0 with %q\n%s", code, hint, out)
		}
		if n := rec.count("enable-linger"); n != 0 {
			t.Errorf("the runner saw enable-linger %d times: %v", n, rec.all())
		}
		_ = home
	})
	t.Run("linger already on", func(t *testing.T) {
		_, rec := unitSandbox(t, "linux")
		rec.linger = "yes"
		code, out := enableCmd(t, "--bind", "127.0.0.1", "--port", strconv.Itoa(freePort(t)), "--yes")
		if code != 0 || strings.Contains(out, "enable-linger") {
			t.Errorf("enable with Linger=yes: exit %d\n%s", code, out)
		}
	})
	t.Run("macOS has no linger", func(t *testing.T) {
		_, rec := unitSandbox(t, "darwin")
		rec.linger = "no"
		_, out := enableCmd(t, "--bind", "127.0.0.1", "--port", strconv.Itoa(freePort(t)), "--yes")
		if strings.Contains(out, "enable-linger") || rec.count("loginctl") != 0 {
			t.Errorf("macOS enable mentioned linger (%v):\n%s", rec.all(), out)
		}
	})
}

func TestEnableNeedsConfigOrBind(t *testing.T) {
	_, rec := unitSandbox(t, "linux")
	code, out := enableCmd(t, "--yes")
	if code != 2 {
		t.Errorf("enable with no config and no --bind: exit %d, want 2\n%s", code, out)
	}
	code, out = enableCmd(t, "--bind", "0.0.0.0", "--yes")
	if code != 2 {
		t.Errorf("enable --bind 0.0.0.0: exit %d, want 2\n%s", code, out)
	}
	if c := rec.all(); len(c) != 0 {
		t.Errorf("commands ran: %v", c)
	}
}

func TestEnableRefusesBinaryThatDoesNotAnswer(t *testing.T) {
	home, rec := unitSandbox(t, "linux")
	bin := filepath.Join(home, ".local", "bin", "crosstalk")
	installWrite(t, bin, "#!/bin/sh\nexit 1\n")
	os.Chmod(bin, 0o755)
	before := installSnapshot(t, home)
	code, out := enableCmd(t, "--bind", "127.0.0.1", "--yes")
	if code != 1 || !strings.Contains(out, bin) {
		t.Errorf("enable with a broken binary: exit %d, want 1 naming %s\n%s", code, bin, out)
	}
	if after := installSnapshot(t, home); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("HOME changed")
	}
	if c := rec.all(); len(c) != 0 {
		t.Errorf("commands ran: %v", c)
	}
}

func TestRemoteCandidatesOrder(t *testing.T) {
	got := orderCandidates([]string{"192.168.1.5", "127.0.0.1", "100.101.102.103", "8.8.8.8", "10.0.0.4", "169.254.1.1", "0.0.0.0", "fe80::1", "100.63.0.1"})
	want := []string{"100.101.102.103", "10.0.0.4", "192.168.1.5"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("orderCandidates = %v, want %v", got, want)
	}
}

func unitK8(t *testing.T, home, bin string) []string {
	t.Helper()
	var lines []string
	for _, r := range installRemoteChecks(home, bin) {
		st := "PASS"
		if !r.OK {
			st = "FAIL"
			if r.WarnOnly {
				st = "WARN"
			}
		}
		lines = append(lines, "K8 "+st+" "+r.Detail)
	}
	return lines
}

func TestK8UnitChecks(t *testing.T) {
	home, rec := unitSandbox(t, "linux")
	bin := filepath.Join(home, ".local", "bin", "crosstalk")
	mustEnable(t, freePort(t))
	if got := strings.Join(unitK8(t, home, bin), "\n"); !strings.HasPrefix(got, "K8 PASS") {
		t.Errorf("healthy unit and gateway:\n%s", got)
	}
	if got := strings.Join(unitK8(t, home, filepath.Join(home, "elsewhere", "crosstalk")), "\n"); !strings.Contains(got, "K8 FAIL") || !strings.Contains(got, "binary") {
		t.Errorf("unit names another binary than the installed one:\n%s", got)
	}
	rec.linger = "no"
	if got := strings.Join(unitK8(t, home, bin), "\n"); !strings.Contains(got, "K8 WARN") || !strings.Contains(got, "loginctl enable-linger") {
		t.Errorf("Linger=no:\n%s", got)
	}
	rec.linger = "yes"
	unit := remoteUnitPath(home)
	installWrite(t, unit, installRead(t, unit)+"\n# edited\n")
	if got := strings.Join(unitK8(t, home, bin), "\n"); !strings.Contains(got, "K8 FAIL") || !strings.Contains(got, unit) {
		t.Errorf("edited unit:\n%s", got)
	}
	if n := rec.count("enable-linger"); n != 0 {
		t.Errorf("K8 ran enable-linger: %v", rec.all())
	}
}

func TestDisableKeepsUnitWhenStopFails(t *testing.T) {
	eachOS(t, func(t *testing.T, goos string) {
		home, rec := unitSandbox(t, goos)
		mustEnable(t, freePort(t))
		unit := remoteUnitPath(home)
		rec.failOn = map[string]string{"darwin": "launchctl bootout", "linux": "disable --now"}[goos]
		code, out := disableCmd(t, "--yes")
		if code != 1 {
			t.Errorf("disable with a failing stop and a live gateway: exit %d, want 1\n%s", code, out)
		}
		cfg, err := remote.LoadConfig(filepath.Join(home, ".config", "crosstalk"))
		if _, serr := os.Stat(unit); serr != nil || err != nil || cfg.UnitSHA256 != fileSHA(t, unit) {
			t.Errorf("unit or hash lost while the gateway still runs: %v %v %+v", serr, err, cfg)
		}
	})
}

func TestEnablePortChangeRestartsUnit(t *testing.T) {
	eachOS(t, func(t *testing.T, goos string) {
		home, rec := unitSandbox(t, goos)
		mustEnable(t, freePort(t))
		next := freePort(t)
		if code, out := enableCmd(t, "--port", strconv.Itoa(next), "--yes"); code != 0 {
			t.Fatalf("enable --port: exit %d\n%s", code, out)
		}
		cfg, _ := remote.LoadConfig(filepath.Join(home, ".config", "crosstalk"))
		if cfg.Port != next {
			t.Fatalf("port = %d, want %d", cfg.Port, next)
		}
		if n := rec.count("launchctl bootstrap") + rec.count("enable --now"); n != 2 {
			t.Errorf("start commands = %d, want 2 (the running gateway kept the old port): %v", n, rec.all())
		}
		if goos == "linux" && rec.count("systemctl --user restart") != 1 {
			t.Errorf("no restart of the running unit: %v", rec.all())
		}
		if !gatewayAnswering(filepath.Join(home, ".config", "crosstalk")) {
			t.Errorf("no gateway answers on the new port %d: %v", next, rec.all())
		}
	})
}

func TestEnableRefusesDanglingUnitSymlink(t *testing.T) {
	eachOS(t, func(t *testing.T, goos string) {
		home, rec := unitSandbox(t, goos)
		unit := remoteUnitPath(home)
		os.MkdirAll(filepath.Dir(unit), 0o755)
		if err := os.Symlink(filepath.Join(home, "nowhere"), unit); err != nil {
			t.Fatal(err)
		}
		code, out := enableCmd(t, "--bind", "127.0.0.1", "--yes")
		if code != 1 || !strings.Contains(out, unit) {
			t.Errorf("enable over a dangling symlink: exit %d, want 1 naming the path\n%s", code, out)
		}
		if fi, err := os.Lstat(unit); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("symlink replaced: %v %v", fi, err)
		}
		if c := rec.all(); len(c) != 0 {
			t.Errorf("commands ran: %v", c)
		}
	})
}
