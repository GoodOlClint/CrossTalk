package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/remote"
)

func init() {
	shimLister = func(string) ([]shimProc, error) { return nil, nil }
}

func fakeShims(t *testing.T, shims ...shimProc) {
	t.Helper()
	old := shimLister
	shimLister = func(string) ([]shimProc, error) { return shims, nil }
	t.Cleanup(func() { shimLister = old })
}

const (
	daemonCmd    = "codex app-server --listen unix://"
	standaloneCm = "codex app-server"
	claudeCmd    = "claude --model sonnet"
)

func k11Lines(rs []harness.CheckResult) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Detail)
	}
	return out
}

func TestK11NeverFails(t *testing.T) {
	fakeShims(t, shimProc{PID: 10, PPID: 2, ParentCmd: daemonCmd, ParentPPID: 1})
	rs := installShimChecks("/bin/crosstalk")
	if len(rs) == 0 {
		t.Fatal("stale shim not reported")
	}
	for _, r := range rs {
		if r.ID != "K11" || r.OK || !r.WarnOnly {
			t.Errorf("K11 result %+v: want a warn-only non-OK check", r)
		}
	}
	var out bytes.Buffer
	if installReport(&out, "crosstalk", rs) {
		t.Errorf("K11 made the checks fail:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "K11 crosstalk WARN") {
		t.Errorf("output %q", out.String())
	}
	var all bytes.Buffer
	if installChecks(context.Background(), &all, nil, harness.InstallEnv{Home: t.TempDir(), Bin: "/bin/crosstalk"}, false) {
		t.Errorf("installChecks failed on a K11 warning:\n%s", all.String())
	}
	if !strings.Contains(all.String(), "K11 crosstalk WARN") {
		t.Errorf("installChecks did not print K11:\n%s", all.String())
	}
}

func TestK11MatchingVersionNotListed(t *testing.T) {
	fakeShims(t,
		shimProc{PID: 11, PPID: 2, ParentCmd: claudeCmd, ParentPPID: 1, Current: true},
		shimProc{PID: 12, PPID: 3, ParentCmd: claudeCmd, ParentPPID: 1})
	rs := installShimChecks("/bin/crosstalk")
	if len(rs) != 1 || rs[0].OK || !strings.Contains(rs[0].Detail, "(pids 12;") {
		t.Fatalf("got %v", k11Lines(rs))
	}
	fakeShims(t, shimProc{PID: 11, PPID: 2, ParentCmd: claudeCmd, ParentPPID: 1, Current: true})
	rs = installShimChecks("/bin/crosstalk")
	if len(rs) != 1 || !rs[0].OK || !rs[0].WarnOnly {
		t.Errorf("all current: %+v", rs)
	}
}

func TestK11GroupsByParent(t *testing.T) {
	fakeShims(t,
		shimProc{PID: 21, PPID: 2, ParentCmd: daemonCmd, ParentPPID: 1},
		shimProc{PID: 22, PPID: 2, ParentCmd: daemonCmd, ParentPPID: 1},
		shimProc{PID: 23, PPID: 4, ParentCmd: standaloneCm, ParentPPID: 900},
		shimProc{PID: 24, PPID: 5, ParentCmd: claudeCmd, ParentPPID: 900},
		shimProc{PID: 25, PPID: 6, ParentCmd: claudeCmd, ParentPPID: 900})
	rs := installShimChecks("/bin/crosstalk")
	if len(rs) != 3 {
		t.Fatalf("want 3 groups, got %v", k11Lines(rs))
	}
	for i, want := range [][]string{{"managed Codex daemon", "pids 21, 22"}, {"standalone Codex app-servers", "pids 23"}, {"Claude Code sessions", "pids 24, 25", "parent pids 5, 6"}} {
		for _, w := range want {
			if !strings.Contains(rs[i].Detail, w) {
				t.Errorf("group %d %q lacks %q", i, rs[i].Detail, w)
			}
		}
	}
}

func TestK11NamesRestartPerGroup(t *testing.T) {
	fakeShims(t,
		shimProc{PID: 21, PPID: 2, ParentCmd: daemonCmd, ParentPPID: 1},
		shimProc{PID: 23, PPID: 4, ParentCmd: standaloneCm, ParentPPID: 900},
		shimProc{PID: 24, PPID: 5, ParentCmd: claudeCmd, ParentPPID: 900})
	rs := installShimChecks("/bin/crosstalk")
	if len(rs) != 3 {
		t.Fatalf("got %v", k11Lines(rs))
	}
	if !strings.Contains(rs[0].Detail, "codex app-server daemon restart") {
		t.Errorf("daemon group: %s", rs[0].Detail)
	}
	for _, i := range []int{1, 2} {
		if strings.Contains(rs[i].Detail, "daemon restart") || !strings.Contains(rs[i].Detail, "session") {
			t.Errorf("group %d: %s", i, rs[i].Detail)
		}
	}
}

func TestK11KillsNothing(t *testing.T) {
	c := exec.Command("sleep", "30")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { c.Wait(); close(exited) }()
	t.Cleanup(func() { c.Process.Kill(); <-exited })
	fakeShims(t, shimProc{PID: c.Process.Pid, PPID: os.Getpid(), ParentCmd: daemonCmd, ParentPPID: 1})
	var out bytes.Buffer
	installChecks(context.Background(), &out, nil, harness.InstallEnv{Home: t.TempDir(), Bin: "/bin/crosstalk"}, false)
	select {
	case <-exited:
		t.Error("the listed process was killed")
	case <-time.After(500 * time.Millisecond):
	}
	if !strings.Contains(out.String(), strconv.Itoa(c.Process.Pid)) {
		t.Errorf("stale pid not reported:\n%s", out.String())
	}
}

func TestClassifyShimParent(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		ppid int
		want shimParentKind
	}{
		{"/opt/homebrew/bin/codex app-server --listen unix://", 1, parentDaemon},
		{"codex app-server", 4461, parentAppServer},
		{"/usr/local/bin/claude --resume", 77, parentClaude},
		{"/bin/zsh -l", 1, parentOther},
		{"", 0, parentOther},
	} {
		if got := classifyShimParent(tc.cmd, tc.ppid); got != tc.want {
			t.Errorf("classifyShimParent(%q, %d) = %d, want %d", tc.cmd, tc.ppid, got, tc.want)
		}
	}
}

func TestParseShimTable(t *testing.T) {
	out := `    1     0 /sbin/launchd
 3602     1 codex app-server
 3672  3602 /opt/homebrew/bin/crosstalk mcp
 3700  3602 /opt/homebrew/bin/crosstalk send x hello
 3701  3602 /usr/bin/vim crosstalk mcp
garbage
`
	got := parseShimTable(out)
	if len(got) != 1 || got[0].PID != 3672 || got[0].PPID != 3602 || got[0].ParentCmd != "codex app-server" || got[0].ParentPPID != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestParseLsofInode(t *testing.T) {
	if n, ok := parseLsofInode("p3672\nftxt\ni450578831\nn/x/crosstalk\nftxt\ni1152921500312613838\nn/usr/lib/dyld\n"); !ok || n != 450578831 {
		t.Errorf("got %d, %v", n, ok)
	}
	if _, ok := parseLsofInode("p1\nfcwd\ni5\n"); ok {
		t.Error("a non-txt entry was read")
	}
}

func TestListShimsFindsRealProcess(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "crosstalk")
	if err := copyFile(t, self, bin); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(bin, "mcp")
	c.Env = append(os.Environ(), "CROSSTALK_TEST_MAIN=1", "HOME="+t.TempDir())
	stdin, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); c.Process.Kill(); c.Wait() })
	find := func() *shimProc {
		for i := 0; i < 50; i++ {
			shims, err := listShims(bin)
			if err != nil {
				t.Fatal(err)
			}
			for j := range shims {
				if shims[j].PID == c.Process.Pid {
					return &shims[j]
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	}
	s := find()
	if s == nil {
		t.Fatal("started crosstalk mcp not listed")
	}
	if !s.Current || s.PPID != os.Getpid() {
		t.Errorf("running the installed file: %+v", s)
	}
	repl := filepath.Join(dir, "new")
	if err := copyFile(t, self, repl); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(repl, bin); err != nil {
		t.Fatal(err)
	}
	if s := find(); s == nil || s.Current {
		t.Errorf("after the file was replaced: %+v", s)
	}
}

func copyFile(t *testing.T, src, dst string) error {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// k8Listener serves the configured gateway port: kind "plain" accepts and
// closes, "other" presents another host's certificate.
func k8Listener(t *testing.T, dir, state, kind string) {
	t.Helper()
	statusLock(t, state)
	l := statusListener(t)
	c, err := remote.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Port = l.Addr().(*net.TCPAddr).Port
	if err := remote.SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if kind == "plain" {
		go func() {
			for {
				cn, err := l.Accept()
				if err != nil {
					return
				}
				cn.Close()
			}
		}()
		return
	}
	other, err := remote.LoadOrCreateHost(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	tl := tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{other.Certificate}, MinVersion: tls.VersionTLS13})
	go func() {
		for {
			cn, err := tl.Accept()
			if err != nil {
				return
			}
			go func() { cn.(*tls.Conn).Handshake(); cn.Close() }()
		}
	}()
}

func k8Line(t *testing.T, home string) string {
	t.Helper()
	_, out := statusChecks(t, home)
	return installCheckLine(out, "K8", "crosstalk")
}

func TestK8DialRefusedSaysPortNotBound(t *testing.T) {
	home, _, state := statusHome(t, true)
	statusLock(t, state)
	l := k8Line(t, home)
	if !strings.Contains(l, "service running, port not bound") || strings.Contains(l, "handshake failed") || strings.Contains(l, "certificate mismatch") {
		t.Errorf("%s", l)
	}
}

func TestK8HandshakeFailedText(t *testing.T) {
	home, dir, state := statusHome(t, true)
	k8Listener(t, dir, state, "plain")
	l := k8Line(t, home)
	if !strings.Contains(l, "handshake failed") || strings.Contains(l, "port not bound") || !strings.Contains(l, "previous CrossTalk version") {
		t.Errorf("%s", l)
	}
}

func TestK8CertificateMismatchText(t *testing.T) {
	home, dir, state := statusHome(t, true)
	k8Listener(t, dir, state, "other")
	l := k8Line(t, home)
	if !strings.Contains(l, "certificate mismatch") || strings.Contains(l, "port not bound") || !strings.Contains(l, "previous CrossTalk version") {
		t.Errorf("%s", l)
	}
}

func TestK8NoPortNamesRestartForUnit(t *testing.T) {
	home, dir, state := statusHome(t, true)
	k8Listener(t, dir, state, "plain")
	want := "launchctl kickstart -k gui/"
	if runtime.GOOS == "linux" {
		want = "systemctl --user restart crosstalk-remote.service"
	}
	if l := k8Line(t, home); strings.Contains(l, want) || !strings.Contains(l, "handshake failed") {
		t.Errorf("no unit: %s", l)
	}
	statusUnit(t, home)
	if l := k8Line(t, home); !strings.Contains(l, want) {
		t.Errorf("unit present, want %q in:\n%s", want, l)
	}
}

func TestK8StateStringsUnchanged(t *testing.T) {
	if remote.StateNoPort != "service running, port not bound" {
		t.Errorf("StateNoPort = %q", remote.StateNoPort)
	}
	_, dir, state := statusHome(t, true)
	k8Listener(t, dir, state, "plain")
	cfg, _ := remote.LoadConfig(dir)
	host, _ := remote.LoadHost(dir)
	if got, err := remote.GatewayStatus(state, cfg, host.Fingerprint); err != nil || got != remote.StateNoPort {
		t.Errorf("GatewayStatus = %q, %v", got, err)
	}
}

func gatewayRunning(t *testing.T, running bool) {
	t.Helper()
	old := gatewayRunsBin
	gatewayRunsBin = func(int, string) bool { return running }
	t.Cleanup(func() { gatewayRunsBin = old })
}

func installWithUnit(t *testing.T) (home string, rec *runLog, args []string) {
	t.Helper()
	home, rec = unitSandbox(t, "linux", "claude")
	args = []string{"--yes", "--bin-dir", filepath.Join(home, "bin")}
	code, out := installRunCmd(t, "install", append([]string{"--remote", "--bind", "127.0.0.1"}, args...)...)
	if code != 0 {
		t.Fatalf("first install: exit %d\n%s", code, out)
	}
	return home, rec, args
}

func TestInstallRestartsGatewayWhenUnitExistsAndStale(t *testing.T) {
	_, rec, args := installWithUnit(t)
	gatewayRunning(t, false)
	before := rec.count("systemctl --user restart")
	code, out := installRunCmd(t, "install", args...)
	if code != 0 || rec.count("systemctl --user restart")-before != 1 || !strings.Contains(out, "restarted the gateway on the installed binary") {
		t.Errorf("exit %d, restarts %d:\n%s", code, rec.count("systemctl --user restart")-before, out)
	}
}

func TestInstallLeavesGatewayWhenCurrent(t *testing.T) {
	_, rec, args := installWithUnit(t)
	gatewayRunning(t, true)
	before := rec.count("systemctl --user restart")
	code, out := installRunCmd(t, "install", args...)
	if code != 0 || rec.count("systemctl --user restart") != before || !strings.Contains(out, "already runs the installed binary") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestInstallNoGatewayRestartWithoutUnit(t *testing.T) {
	home, rec := unitSandbox(t, "linux", "claude")
	gatewayRunning(t, false)
	code, out := installRunCmd(t, "install", "--yes", "--bin-dir", filepath.Join(home, "bin"))
	if code != 0 || len(rec.all()) != 0 || strings.Contains(out, "== gateway") {
		t.Errorf("exit %d, calls %v:\n%s", code, rec.all(), out)
	}
}

func TestInstallPrintsRestartAdviceOnUpgrade(t *testing.T) {
	home, rec := unitSandbox(t, "linux", "claude")
	fakeShims(t, shimProc{PID: 31, PPID: 3, ParentCmd: daemonCmd, ParentPPID: 1})
	code, out := installRunCmd(t, "install", "--yes", "--bin-dir", filepath.Join(home, "bin"))
	if code != 0 || !strings.Contains(out, "K11 crosstalk WARN") || !strings.Contains(out, "codex app-server daemon restart") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if rec.count("config/mcpServer/reload") != 0 || strings.Contains(out, "mcpServer/reload") {
		t.Errorf("install sent or named a reload:\n%s", out)
	}
}

func TestInstallNoAdviceWhenShimsCurrent(t *testing.T) {
	home, _ := unitSandbox(t, "linux", "claude")
	fakeShims(t, shimProc{PID: 31, PPID: 3, ParentCmd: daemonCmd, ParentPPID: 1, Current: true})
	code, out := installRunCmd(t, "install", "--yes", "--bin-dir", filepath.Join(home, "bin"))
	if code != 0 || strings.Contains(out, "K11 crosstalk WARN") || strings.Contains(out, "daemon restart") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}
