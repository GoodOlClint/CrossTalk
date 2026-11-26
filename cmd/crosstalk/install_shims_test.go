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
		{"/opt/homebrew/bin/opencode serve --service", 1, parentOpenCode},
		{"opencode serve --service --port 0", 88, parentOpenCode},
		{"/opt/homebrew/bin/opencode", 88, parentOther},
		{"/opt/homebrew/bin/opencode serve", 88, parentOther},
		{"opencode run serve --service", 88, parentOther},
		{"opencode run --service serve", 88, parentOther},
		{"pi", 88, parentPi},
		{"pi-foo", 88, parentOther},
		{"/usr/bin/pi", 88, parentOther},
		{"pi --mode rpc", 88, parentOther},
		{"/bin/zsh -l", 1, parentOther},
		{"", 0, parentOther},
	} {
		if got := classifyShimParent(tc.cmd, tc.ppid); got != tc.want {
			t.Errorf("classifyShimParent(%q, %d) = %d, want %d", tc.cmd, tc.ppid, got, tc.want)
		}
	}
}

func TestK11GroupsOpenCodeServiceParent(t *testing.T) {
	fakeShims(t,
		shimProc{PID: 21, PPID: 5, ParentCmd: "/opt/homebrew/bin/opencode serve --service", ParentPPID: 1},
		shimProc{PID: 22, PPID: 5, ParentCmd: "/opt/homebrew/bin/opencode serve --service", ParentPPID: 1})
	rs := installShimChecks("/bin/crosstalk")
	if len(rs) != 1 || rs[0].OK || !rs[0].WarnOnly {
		t.Fatalf("got %v", k11Lines(rs))
	}
	d := rs[0].Detail
	for _, want := range []string{"2 crosstalk mcp process(es) under the OpenCode service", "pids 21, 22", "restart the OpenCode service", "TUIs reconnect", "ends running turns"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail %q lacks %q", d, want)
		}
	}
	if strings.Contains(d, "other parents") {
		t.Errorf("OpenCode shims fell into other parents: %q", d)
	}
}

func TestK11GroupsPiParent(t *testing.T) {
	fakeShims(t,
		shimProc{PID: 31, PPID: 6, ParentCmd: "pi", ParentPPID: 1},
		shimProc{PID: 32, PPID: 7, ParentCmd: "pi", ParentPPID: 1})
	rs := installShimChecks("/bin/crosstalk")
	if len(rs) != 1 || rs[0].OK || !rs[0].WarnOnly {
		t.Fatalf("got %v", k11Lines(rs))
	}
	d := rs[0].Detail
	for _, want := range []string{"2 crosstalk mcp process(es) under Pi sessions", "pids 31, 32", "restart each Pi session", "/reload"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail %q lacks %q", d, want)
		}
	}
	if strings.Contains(d, "other parents") {
		t.Errorf("Pi shims fell into other parents: %q", d)
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

func TestParseLsofFile(t *testing.T) {
	if id, ok := parseLsofFile("p3672\nftxt\nD0x1000010\ni450578831\nn/x/crosstalk\nftxt\nD0x1\ni1152921500312613838\nn/usr/lib/dyld\n"); !ok || id != (fileID{0x1000010, 450578831}) {
		t.Errorf("got %+v, %v", id, ok)
	}
	if _, ok := parseLsofFile("p1\nfcwd\nD0x1\ni5\n"); ok {
		t.Error("a non-txt entry was read")
	}
	if _, ok := parseLsofFile("p1\nftxt\ni5\n"); ok {
		t.Error("an entry without a device was accepted")
	}
}

func TestProcRunsBinComparesDeviceAndInode(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "crosstalk")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(bin)
	want, ok := idOf(fi)
	if !ok {
		t.Fatal("no Stat_t")
	}
	old := procFile
	t.Cleanup(func() { procFile = old })
	for _, tc := range []struct {
		name string
		got  fileID
		runs bool
	}{
		{"same file", want, true},
		{"same inode, other device", fileID{want.Dev + 1, want.Ino}, false},
		{"same device, other inode", fileID{want.Dev, want.Ino + 1}, false},
	} {
		procFile = func(int) (fileID, bool) { return tc.got, true }
		if runs, known := procRunsBin(1, bin); !known || runs != tc.runs {
			t.Errorf("%s: runs=%v known=%v", tc.name, runs, known)
		}
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
	c.Env = shimEnv(t.TempDir(), "CROSSTALK_TEST_MAIN=1")
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
	gatewayRunsBin = func(int, string) (bool, bool) { return running, true }
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

func TestInstallRestartsGatewayWhenBinaryUnknown(t *testing.T) {
	_, rec, args := installWithUnit(t)
	old := gatewayRunsBin
	gatewayRunsBin = func(int, string) (bool, bool) { return false, false }
	t.Cleanup(func() { gatewayRunsBin = old })
	before := rec.count("systemctl --user restart")
	code, out := installRunCmd(t, "install", args...)
	if code != 0 || rec.count("systemctl --user restart")-before != 1 || strings.Contains(out, "already runs") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

func TestInstallLeavesGatewayWhenUnitRunsOtherBinary(t *testing.T) {
	home, rec, args := installWithUnit(t)
	gatewayRunning(t, false)
	other := filepath.Join(home, "elsewhere", "crosstalk")
	p := remoteUnitPath(home)
	if err := os.WriteFile(p, unitContent(remoteGOOS, other, home), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setUnitHash(filepath.Join(home, ".config", "crosstalk"), sha256Hex(unitContent(remoteGOOS, other, home))); err != nil {
		t.Fatal(err)
	}
	before := rec.count("systemctl --user restart")
	_, out := installRunCmd(t, "install", args...)
	if rec.count("systemctl --user restart") != before || !strings.Contains(out, "runs a binary other than") {
		t.Errorf("restarted a unit that runs another binary:\n%s", out)
	}
}

func TestInstallBinaryKeepsIdenticalCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "bin", "crosstalk")
	if err := os.WriteFile(src, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if skipped, err := installBinary(src, dst); err != nil || skipped {
		t.Fatalf("first copy: skipped=%v err=%v", skipped, err)
	}
	before, _ := os.Stat(dst)
	if skipped, err := installBinary(src, dst); err != nil || !skipped {
		t.Fatalf("second copy: skipped=%v err=%v", skipped, err)
	}
	if after, _ := os.Stat(dst); !os.SameFile(before, after) {
		t.Error("an identical binary was replaced by a new inode")
	}
	os.WriteFile(src, []byte("binary2"), 0o755)
	if skipped, err := installBinary(src, dst); err != nil || skipped {
		t.Fatalf("changed source: skipped=%v err=%v", skipped, err)
	}
}

func TestK11ReportsUnidentifiedProcesses(t *testing.T) {
	fakeShims(t, shimProc{PID: 41, PPID: 2, ParentCmd: claudeCmd, ParentPPID: 1, Unknown: true})
	rs := installShimChecks("/bin/crosstalk")
	if len(rs) != 1 || rs[0].OK || !rs[0].WarnOnly || !strings.Contains(rs[0].Detail, "could not tell") || !strings.Contains(rs[0].Detail, "pids 41") {
		t.Errorf("got %v", k11Lines(rs))
	}
}

func TestK11DaemonAdviceCoversOrphanedAppServer(t *testing.T) {
	fakeShims(t, shimProc{PID: 21, PPID: 2, ParentCmd: daemonCmd, ParentPPID: 1})
	rs := installShimChecks("/bin/crosstalk")
	if len(rs) != 1 || !strings.Contains(rs[0].Detail, "you started yourself") {
		t.Errorf("got %v", k11Lines(rs))
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

func ownedUnitHome(t *testing.T) (home string, rec *runLog, bin string) {
	t.Helper()
	home, _, _ = statusHome(t, true)
	statusUnit(t, home)
	rec = &runLog{}
	oldRun := unitRun
	unitRun = rec.run
	t.Cleanup(func() { unitRun = oldRun })
	gatewayRunning(t, false)
	return home, rec, filepath.Join(home, "bin", "crosstalk")
}

func TestGatewayRestartStepSkipsWhenLockNotHeld(t *testing.T) {
	home, rec, bin := ownedUnitHome(t)
	state := filepath.Join(home, ".local", "state", "crosstalk")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "gateway.lock"), []byte("99999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	step := gatewayRestartStep(home, filepath.Join(home, ".config", "crosstalk"), state, bin, &out)
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := rec.all(); len(c) != 0 || !strings.Contains(out.String(), "no gateway is running") {
		t.Errorf("calls %v:\n%s", c, out.String())
	}
}

func TestGatewayRestartStepSkipsUnitNotOwned(t *testing.T) {
	home, rec, bin := ownedUnitHome(t)
	dir := filepath.Join(home, ".config", "crosstalk")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	holdGatewayLock(t, state)
	if err := setUnitHash(dir, "not-the-hash-of-this-file"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := gatewayRestartStep(home, dir, state, bin, &out).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := rec.all(); len(c) != 0 || !strings.Contains(out.String(), "runs a binary other than") {
		t.Errorf("restarted a unit CrossTalk did not write; calls %v:\n%s", c, out.String())
	}
}

func TestInstallPlansNoGatewayStepForUnitNotOwned(t *testing.T) {
	home, rec := unitSandbox(t, "linux", "claude")
	gatewayRunning(t, false)
	p := remoteUnitPath(home)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, unitContent(remoteGOOS, filepath.Join(home, "bin", "crosstalk"), home), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out := installRunCmd(t, "install", "--yes", "--bin-dir", filepath.Join(home, "bin"))
	if !strings.Contains(out, "== checks") || strings.Contains(out, "== gateway") || len(rec.all()) != 0 {
		t.Errorf("calls %v:\n%s", rec.all(), out)
	}
}

func TestK11CodexNotesNewThreads(t *testing.T) {
	fakeShims(t,
		shimProc{PID: 21, PPID: 2, ParentCmd: daemonCmd, ParentPPID: 1},
		shimProc{PID: 23, PPID: 4, ParentCmd: standaloneCm, ParentPPID: 900},
		shimProc{PID: 24, PPID: 5, ParentCmd: claudeCmd, ParentPPID: 900})
	rs := installShimChecks("/bin/crosstalk")
	if len(rs) != 3 {
		t.Fatalf("got %v", k11Lines(rs))
	}
	const note = "only in threads started after the upgrade"
	for _, i := range []int{0, 1} {
		if !strings.Contains(rs[i].Detail, note) {
			t.Errorf("Codex group %d lacks %q: %s", i, note, rs[i].Detail)
		}
	}
	if strings.Contains(rs[2].Detail, note) {
		t.Errorf("Claude group carries the Codex note: %s", rs[2].Detail)
	}
	for _, off := range []bool{false, true} {
		if off {
			t.Setenv("CROSSTALK_NO_AUTO_REFRESH", "1")
		}
		if got := installShimChecks("/bin/crosstalk"); len(got) != 3 || got[0].OK || !strings.Contains(got[0].Detail, "pids 21") {
			t.Errorf("shim that cannot refresh (off=%v) not listed: %v", off, k11Lines(got))
		}
	}
}
