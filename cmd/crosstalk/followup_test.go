package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
	"github.com/GoodOlClint/crosstalk/internal/quiesce"
	"github.com/GoodOlClint/crosstalk/internal/refresh"
	"github.com/GoodOlClint/crosstalk/internal/upgrade"
)

const fuVer = "9.9.9"

// fuRec is a fake harness CLI: it records each command and keeps the
// installed plugin versions, which the plugin commands set to fuVer, or
// leave at 0.0.1 when stale.
type fuRec struct {
	mu        sync.Mutex
	calls     []string
	home      string
	stale     bool
	claudeVer string
}

func (f *fuRec) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := strings.Join(args, " ")
	f.calls = append(f.calls, name+" "+a)
	v := fuVer
	if f.stale {
		v = "0.0.1"
	}
	switch name + " " + a {
	case "claude plugin list --json":
		if f.claudeVer == "" {
			return []byte("[]"), nil
		}
		return json.Marshal([]map[string]string{{"id": "crosstalk@crosstalk", "version": f.claudeVer}})
	case "claude plugin install crosstalk@crosstalk", "claude plugin update crosstalk@crosstalk":
		f.claudeVer = v
	case "codex plugin add crosstalk@crosstalk":
		p := filepath.Join(f.home, ".codex", "plugins", "cache", "crosstalk", "crosstalk", v, ".codex-plugin", "plugin.json")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(`{"version":"`+v+`"}`), 0o644)
	}
	return nil, nil
}

func (f *fuRec) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// fuSandbox points HOME (unless set already by the caller) and CODEX_HOME
// at temp dirs and puts fake claude and codex on PATH, plus a SHELL, all of
// which record being started in home/started.
func fuSandbox(t *testing.T, home string, setUp ...string) (*fuRec, harness.InstallEnv, []harness.Harness) {
	t.Helper()
	if home == "" {
		home = t.TempDir()
		t.Setenv("HOME", home)
	}
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	bin := t.TempDir()
	for _, c := range []string{"claude", "codex", "sh"} {
		script := "#!/bin/sh\necho " + c + " >> " + filepath.Join(home, "started") + "\nexit 3\n"
		if err := os.WriteFile(filepath.Join(bin, c), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("SHELL", filepath.Join(bin, "sh"))
	rec := &fuRec{home: home}
	for _, h := range setUp {
		switch h {
		case "claude":
			p := filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(`{"plugins":{"crosstalk@crosstalk":[]}}`), 0o644)
			os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"mcpServers":{"crosstalk":{"type":"stdio","command":"`+filepath.Join(home, "bin", "crosstalk")+`","args":["mcp"]}}}`), 0o644)
			rec.claudeVer = "0.0.1"
		case "codex":
			p := filepath.Join(home, ".codex", "config.toml")
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte("[mcp_servers.crosstalk]\ncommand = \""+filepath.Join(home, "bin", "crosstalk")+"\"\nargs = [\"mcp\"]\n"), 0o644)
		}
	}
	state := filepath.Join(home, ".local", "state", "crosstalk")
	hs, err := harnesses(state)
	if err != nil {
		t.Fatal(err)
	}
	env := harness.InstallEnv{
		Bin: filepath.Join(home, "bin", "crosstalk"), Version: fuVer, Home: home, Run: rec.run,
		Handshake: func(context.Context, string, []string, []string) error {
			t.Error("the follow-up started the registered command (K6)")
			return errors.New("not allowed")
		},
		Marketplace: "GoodOlClint/CrossTalk", StateDir: state, Out: io.Discard,
	}
	old := followUpSettle
	followUpSettle = 0
	t.Cleanup(func() { followUpSettle = old })
	return rec, env, hs
}

// fuBound is a shim that has already bound a session.
var fuBound = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

func fuCfgDir(env harness.InstallEnv) string { return filepath.Join(env.Home, ".config", "crosstalk") }

func fuRun(env harness.InstallEnv, hs []harness.Harness, notice func(string)) {
	r := followUpRunner(env, hs, fuCfgDir(env), notice)
	r.Interval = 0
	r.Run(context.Background())
}

func fuDone(env harness.InstallEnv, target string) bool {
	return upgrade.Done(env.StateDir, env.Version, target)
}

func TestFollowUpOnlyPluginAndGateway(t *testing.T) {
	home, unitRec, _ := ownedUnitHome(t)
	holdGatewayLock(t, filepath.Join(home, ".local", "state", "crosstalk"))
	var restarted atomic.Bool
	unitRec.hook = func(call string) {
		if strings.Contains(call, "restart") || strings.Contains(call, "kickstart") {
			restarted.Store(true)
		}
	}
	old := gatewayRunsBin
	gatewayRunsBin = func(int, string) (bool, bool) { return restarted.Load(), true }
	t.Cleanup(func() { gatewayRunsBin = old })

	rec, env, hs := fuSandbox(t, home, "claude", "codex")
	files := []string{filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".claude.json"), filepath.Join(home, ".codex", "config.toml")}
	os.WriteFile(files[0], []byte(`{"permissions":{"allow":[]}}`), 0o644)
	before := map[string]string{}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		before[f] = string(b)
	}
	fuRun(env, hs, nil)

	allowed := []string{
		"claude plugin marketplace add ", "claude plugin marketplace update ", "claude plugin install ", "claude plugin update ", "claude plugin list --json",
		"codex plugin marketplace add ", "codex plugin marketplace upgrade ", "codex plugin add ",
	}
	for _, c := range rec.all() {
		if !slices.ContainsFunc(allowed, func(p string) bool { return strings.HasPrefix(c, p) }) {
			t.Errorf("follow-up ran %q", c)
		}
	}
	for _, c := range unitRec.all() {
		if !strings.Contains(c, "systemctl --user restart") && !strings.Contains(c, "launchctl kickstart -k") {
			t.Errorf("follow-up ran unit command %q", c)
		}
	}
	if !restarted.Load() {
		t.Error("the gateway on an older binary was not restarted")
	}
	for _, f := range files {
		if b, _ := os.ReadFile(f); string(b) != before[f] {
			t.Errorf("follow-up changed %s", f)
		}
	}
	if b, err := os.ReadFile(filepath.Join(home, "started")); err == nil {
		t.Errorf("follow-up started a process outside env.Run (login shell or harness CLI): %s", b)
	}
	for _, tg := range []string{"claude", "codex", "gateway"} {
		if !fuDone(env, tg) {
			t.Errorf("no %s marker after a verified run", tg)
		}
	}
}

func TestFollowUpNoMarkerWhenK1Fails(t *testing.T) {
	rec, env, hs := fuSandbox(t, "", "claude", "codex")
	rec.stale = true
	fuRun(env, hs, nil)
	if fuDone(env, "claude") || fuDone(env, "codex") {
		t.Fatalf("marker written though the plugin stayed at the old version: claude=%v codex=%v", fuDone(env, "claude"), fuDone(env, "codex"))
	}
	if n := slices.Index(rec.all(), "claude plugin update crosstalk@crosstalk"); n < 0 {
		t.Fatalf("plugin step did not run: %v", rec.all())
	}
	rec.stale = false
	fuRun(env, hs, nil)
	if !fuDone(env, "claude") || !fuDone(env, "codex") {
		t.Fatalf("next start did not retry: claude=%v codex=%v", fuDone(env, "claude"), fuDone(env, "codex"))
	}
}

func TestFollowUpPerHarnessMarkers(t *testing.T) {
	rec, env, hs := fuSandbox(t, "", "claude")
	fuRun(env, hs, nil)
	if !fuDone(env, "claude") {
		t.Error("no claude marker")
	}
	if fuDone(env, "codex") {
		t.Error("codex marker written for a harness that is not set up")
	}
	for _, c := range rec.all() {
		if strings.HasPrefix(c, "codex ") {
			t.Errorf("ran %q for a harness that is not set up", c)
		}
	}
	if slices.ContainsFunc(followUpTargets(env, hs, fuCfgDir(env)), func(tg upgrade.Target) bool { return tg.Name == "gateway" }) {
		t.Error("gateway target without a login unit")
	}
}

func TestFollowUpSkipsHarnessNotFound(t *testing.T) {
	rec, env, hs := fuSandbox(t, "", "claude", "codex")
	os.Remove(filepath.Join(os.Getenv("PATH"), "codex"))
	fuRun(env, hs, nil)
	if fuDone(env, "codex") || slices.ContainsFunc(rec.all(), func(c string) bool { return strings.HasPrefix(c, "codex ") }) {
		t.Errorf("ran codex steps with codex not on PATH: %v", rec.all())
	}
}

func gatewayHome(t *testing.T, runs func(restarted bool) (bool, bool)) (*runLog, harness.InstallEnv, []harness.Harness, *atomic.Bool) {
	t.Helper()
	home, unitRec, _ := ownedUnitHome(t)
	holdGatewayLock(t, filepath.Join(home, ".local", "state", "crosstalk"))
	var restarted atomic.Bool
	unitRec.hook = func(call string) { restarted.Store(true) }
	old := gatewayRunsBin
	gatewayRunsBin = func(int, string) (bool, bool) { return runs(restarted.Load()) }
	t.Cleanup(func() { gatewayRunsBin = old })
	_, env, hs := fuSandbox(t, home)
	return unitRec, env, hs, &restarted
}

func TestFollowUpGatewayLeftAloneOnInstalledBinary(t *testing.T) {
	unitRec, env, hs, _ := gatewayHome(t, func(bool) (bool, bool) { return true, true })
	fuRun(env, hs, nil)
	if c := unitRec.all(); len(c) != 0 {
		t.Errorf("restarted a gateway already on the installed binary: %v", c)
	}
	if !fuDone(env, "gateway") {
		t.Error("no gateway marker for a gateway on the installed binary")
	}
}

func TestFollowUpGatewayNoMarkerUntilItRunsInstalled(t *testing.T) {
	unitRec, env, hs, _ := gatewayHome(t, func(bool) (bool, bool) { return false, true })
	fuRun(env, hs, nil)
	if len(unitRec.all()) != 1 {
		t.Errorf("unit calls %v, want one restart", unitRec.all())
	}
	if fuDone(env, "gateway") {
		t.Error("gateway marker written while it still runs another binary")
	}
}

func TestFollowUpGatewayNotRestartedWhenUnknown(t *testing.T) {
	unitRec, env, hs, _ := gatewayHome(t, func(bool) (bool, bool) { return false, false })
	fuRun(env, hs, nil)
	if c := unitRec.all(); len(c) != 0 {
		t.Errorf("restarted a gateway whose binary is unknown: %v", c)
	}
	if fuDone(env, "gateway") {
		t.Error("gateway marker without a verified binary")
	}
}

func TestFollowUpGatewayNotRunning(t *testing.T) {
	home, unitRec, _ := ownedUnitHome(t)
	_, env, hs := fuSandbox(t, home)
	fuRun(env, hs, nil)
	if c := unitRec.all(); len(c) != 0 {
		t.Errorf("unit calls with no gateway running: %v", c)
	}
	if fuDone(env, "gateway") {
		t.Error("gateway marker with no gateway running")
	}
}

func TestFollowUpOffAndNotArmed(t *testing.T) {
	rec, env, hs := fuSandbox(t, "", "claude")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, ok := startFollowUp(ctx, false, fuBound, env, hs, fuCfgDir(env), nil); ok {
		t.Error("started with refresh not armed")
	}
	t.Setenv(refresh.OffEnv, "1")
	if _, ok := startFollowUp(ctx, true, fuBound, env, hs, fuCfgDir(env), nil); ok {
		t.Errorf("started with %s=1", refresh.OffEnv)
	}
	time.Sleep(100 * time.Millisecond)
	if c := rec.all(); len(c) != 0 {
		t.Fatalf("commands ran while off: %v", c)
	}
	t.Setenv(refresh.OffEnv, "0")
	stop, ok := startFollowUp(ctx, true, fuBound, env, hs, fuCfgDir(env), nil)
	if !ok {
		t.Fatal("did not start when armed")
	}
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for !fuDone(env, "claude") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !fuDone(env, "claude") {
		t.Fatal("armed follow-up wrote no marker")
	}
}

func TestFollowUpNoticeNamesTargets(t *testing.T) {
	rec, env, hs := fuSandbox(t, "", "claude", "codex")
	var notices []string
	env.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "codex" && len(args) > 1 && args[1] == "add" {
			return nil, errors.New("codex: network unreachable")
		}
		return rec.run(ctx, name, args...)
	}
	fuRun(env, hs, func(n string) { notices = append(notices, n) })
	if len(notices) != 1 {
		t.Fatalf("notices = %q", notices)
	}
	for _, want := range []string{"claude", "codex", "network unreachable", "run `crosstalk install`"} {
		if !strings.Contains(notices[0], want) {
			t.Errorf("notice %q lacks %q", notices[0], want)
		}
	}
}

func TestInstallWritesMarkerForItsTargets(t *testing.T) {
	home := installSandbox(t, "claude", "codex")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	v := mcpserver.Version
	args := []string{"--yes", "--bin-dir", filepath.Join(home, "bin")}

	rel, ok, err := upgrade.TryLock(state)
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	var out syncBuf
	code := make(chan int, 1)
	go func() {
		code <- runInstall("install", append([]string{"claude"}, args...), strings.NewReader(""), false, &out, &out)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "upgrade.lock") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "upgrade.lock") {
		t.Fatalf("install printed no waiting message while the lock was held:\n%s", out.String())
	}
	if upgrade.Done(state, v, "claude") {
		t.Fatal("marker written while another process held the lock")
	}
	select {
	case c := <-code:
		t.Fatalf("install finished (exit %d) while the lock was held:\n%s", c, out.String())
	case <-time.After(200 * time.Millisecond):
	}
	rel()
	if c := <-code; c != 0 {
		t.Fatalf("install claude: exit %d\n%s", c, out.String())
	}
	if !upgrade.Done(state, v, "claude") {
		t.Errorf("no claude marker after a verified install:\n%s", out.String())
	}
	if upgrade.Done(state, v, "codex") {
		t.Error("codex marker written by an install of claude only")
	}

	t.Setenv("INSTALL_FAKE_FAIL", "codex plugin add crosstalk@crosstalk")
	if c, o := installRunCmd(t, "install", append([]string{"codex"}, args...)...); c == 0 {
		t.Fatalf("install codex with a failing plugin step: exit 0\n%s", o)
	}
	if upgrade.Done(state, v, "codex") {
		t.Error("codex marker written for a failed install")
	}
	if _, ok, _ := upgrade.TryLock(state); !ok {
		t.Error("install left the upgrade lock held")
	}
}

func TestInstallNoMarkerWhenK1Fails(t *testing.T) {
	home := installSandbox(t, "claude")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	t.Setenv("INSTALL_FAKE_PLUGIN_VERSION", "0.0.1")
	installRunCmd(t, "install", "--yes", "--bin-dir", filepath.Join(home, "bin"))
	if upgrade.Done(state, mcpserver.Version, "claude") {
		t.Error("claude marker written though K1 could not pass")
	}
}

func TestFollowUpSetUpErrorNotATarget(t *testing.T) {
	rec, env, hs := fuSandbox(t, "", "codex")
	p := filepath.Join(env.Home, ".claude", "plugins", "installed_plugins.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("{not json"), 0o644)
	fuRun(env, hs, nil)
	for _, c := range rec.all() {
		if strings.HasPrefix(c, "claude ") {
			t.Errorf("ran %q for a harness whose setup could not be read", c)
		}
	}
	if !fuDone(env, "codex") {
		t.Error("codex was not updated")
	}
}

func TestFollowUpSkipsHarnessRegisteredElsewhere(t *testing.T) {
	rec, env, hs := fuSandbox(t, "", "claude", "codex")
	os.WriteFile(filepath.Join(env.Home, ".codex", "config.toml"), []byte("[mcp_servers.crosstalk]\ncommand = \"/elsewhere/crosstalk\"\nargs = [\"mcp\"]\n"), 0o644)
	fuRun(env, hs, nil)
	for _, c := range rec.all() {
		if strings.HasPrefix(c, "codex ") {
			t.Errorf("ran %q for a harness registered to another binary", c)
		}
	}
}

func TestFollowUpNoMarkerWhenRegistrationLost(t *testing.T) {
	rec, env, hs := fuSandbox(t, "", "codex")
	cfg := filepath.Join(env.Home, ".codex", "config.toml")
	env.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := rec.run(ctx, name, args...)
		if name == "codex" && strings.Join(args, " ") == "plugin add crosstalk@crosstalk" {
			os.WriteFile(cfg, []byte("model = \"x\"\n"), 0o644)
		}
		return out, err
	}
	var notices []string
	fuRun(env, hs, func(n string) { notices = append(notices, n) })
	if fuDone(env, "codex") {
		t.Fatal("codex marker written though the plugin step dropped the registration")
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "run `crosstalk install`") {
		t.Fatalf("notices = %q", notices)
	}
}

func TestFollowUpCountedForRefreshAndJoinedOnStop(t *testing.T) {
	rec, env, hs := fuSandbox(t, "", "claude")
	inStep := make(chan struct{})
	var busy atomic.Int32
	var finished atomic.Bool
	env.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "plugin marketplace add GoodOlClint/CrossTalk" {
			busy.Store(int32(quiesce.Busy()))
			close(inStep)
			<-ctx.Done()
			finished.Store(true)
			return nil, ctx.Err()
		}
		return rec.run(ctx, name, args...)
	}
	stop, ok := startFollowUp(context.Background(), true, fuBound, env, hs, fuCfgDir(env), nil)
	if !ok {
		t.Fatal("not started")
	}
	select {
	case <-inStep:
	case <-time.After(5 * time.Second):
		t.Fatal("step never ran")
	}
	if busy.Load() < 1 {
		t.Error("a running follow-up is not counted as work refresh quiescence waits for")
	}
	stop()
	if !finished.Load() {
		t.Error("stop returned before the running step ended")
	}
	if quiesce.Busy() != 0 {
		t.Errorf("quiesce count %d after stop", quiesce.Busy())
	}
}

func TestInstallHandshakeTurnsFollowUpOff(t *testing.T) {
	dir := t.TempDir()
	dump := filepath.Join(dir, "env")
	script := filepath.Join(dir, "crosstalk")
	os.WriteFile(script, []byte("#!/bin/sh\nenv > "+dump+"\nexit 1\n"), 0o755)
	installHandshake(context.Background(), script, []string{"mcp"}, []string{refresh.OffEnv + "=0"})
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	var ours []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, refresh.OffEnv+"=") {
			ours = append(ours, l)
		}
	}
	if !slices.Equal(ours, []string{refresh.OffEnv + "=1"}) {
		t.Fatalf("the handshake child can run the follow-up: %q", ours)
	}
}

func TestUninstallTakesUpgradeLock(t *testing.T) {
	home := installSandbox(t, "claude")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	args := []string{"--yes", "--bin-dir", filepath.Join(home, "bin")}
	if c, o := installRunCmd(t, "install", args...); c != 0 {
		t.Fatalf("install: %d\n%s", c, o)
	}
	rel, ok, err := upgrade.TryLock(state)
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	var out syncBuf
	code := make(chan int, 1)
	go func() { code <- runInstall("uninstall", args, strings.NewReader(""), false, &out, &out) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "upgrade.lock") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case c := <-code:
		t.Fatalf("uninstall finished (exit %d) while the lock was held:\n%s", c, out.String())
	default:
	}
	rel()
	if c := <-code; c != 0 {
		t.Fatalf("uninstall: %d\n%s", c, out.String())
	}
}

func TestStartRefreshNotArmedWhenWatcherCannotStart(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin", "crosstalk")
	reg := `{"mcpServers":{"crosstalk":{"type":"stdio","command":"` + bin + `","args":["mcp"]}}}`
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(reg), 0o600)
	hs, err := harnesses(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = append([]string{bin}, oldArgs[1:]...)
	if _, _, armed := startRefresh(context.Background(), home, hs, nil); armed {
		t.Error("armed although the trusted path cannot be stat'd")
	}
	os.MkdirAll(filepath.Dir(bin), 0o755)
	os.WriteFile(bin, []byte("x"), 0o755)
	old := pollStdin
	pollStdin = func() (*os.File, error) { return nil, errors.New("no pause") }
	t.Cleanup(func() { pollStdin = old })
	if _, _, armed := startRefresh(context.Background(), home, hs, nil); armed {
		t.Error("armed although stdin cannot be paused")
	}
	pollStdin = old
}

func TestFollowUpRetryDropsUninstalledHarness(t *testing.T) {
	rec, env, hs := fuSandbox(t, "", "codex")
	rec.stale = true
	r := followUpRunner(env, hs, fuCfgDir(env), nil)
	cfg := filepath.Join(env.Home, ".codex", "config.toml")
	var waits int
	r.After = func(time.Duration) <-chan time.Time {
		waits++
		os.WriteFile(cfg, []byte("model = \"x\"\n"), 0o644)
		c := make(chan time.Time, 1)
		c <- time.Time{}
		return c
	}
	r.Run(context.Background())
	if n := slices.Index(rec.all(), "codex plugin add crosstalk@crosstalk"); n < 0 || strings.Count(strings.Join(rec.all(), "\n"), "codex plugin add") != 1 {
		t.Fatalf("a retry ran the plugin step for a harness uninstalled meanwhile: %v", rec.all())
	}
	if waits != 1 {
		t.Errorf("waits = %d, want 1", waits)
	}
}

func TestFollowUpRunnerBounds(t *testing.T) {
	r := followUpRunner(harness.InstallEnv{}, nil, "", nil)
	if r.TargetTimeout <= 0 || r.Enter == nil || r.Interval != 15*time.Minute || r.Bound != 6*time.Hour {
		t.Fatalf("runner timeout %v, enter set %v, interval %v, bound %v", r.TargetTimeout, r.Enter != nil, r.Interval, r.Bound)
	}
}

func TestFollowUpRetryDropsRemovedUnit(t *testing.T) {
	unitRec, env, hs, _ := gatewayHome(t, func(bool) (bool, bool) { return false, true })
	r := followUpRunner(env, hs, fuCfgDir(env), nil)
	var waits int
	r.After = func(time.Duration) <-chan time.Time {
		waits++
		os.Remove(remoteUnitPath(env.Home))
		c := make(chan time.Time, 1)
		c <- time.Time{}
		return c
	}
	r.Run(context.Background())
	if n := len(unitRec.all()); n != 1 || waits != 1 {
		t.Fatalf("unit calls %v, waits %d: a retry restarted a gateway whose unit was removed", unitRec.all(), waits)
	}
}

func TestShimTestsKeepHarnessCLIsOffPath(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []string{"claude", "codex"} {
		os.WriteFile(filepath.Join(dir, c), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var path string
	for _, e := range shimEnv(t.TempDir()) {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			path = v
		}
	}
	for _, c := range []string{"claude", "codex"} {
		for _, d := range filepath.SplitList(path) {
			if _, err := os.Stat(filepath.Join(d, c)); err == nil {
				t.Errorf("a test shim's PATH finds %s in %s: its follow-up would run the real CLI", c, d)
			}
		}
	}
}

// A health-check shim (`claude mcp get`, `claude mcp list`) serves no session
// and exits within a second; it must not start the follow-up (#264).
func TestFollowUpProbeShimStartsNothing(t *testing.T) {
	t.Setenv(refresh.OffEnv, "")
	rec, env, hs := fuSandbox(t, "", "claude", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	bound := make(chan struct{})
	stop, _ := startFollowUp(ctx, true, bound, env, hs, fuCfgDir(env), nil)
	<-ctx.Done()
	stop()
	if c := rec.all(); len(c) != 0 {
		t.Errorf("a target ran for a shim that served no session: %v", c)
	}
	if _, err := os.Stat(upgrade.LogPath(env.StateDir, env.Version)); err == nil {
		b, _ := os.ReadFile(upgrade.LogPath(env.StateDir, env.Version))
		t.Errorf("follow-up.log written for a shim that served no session:\n%s", b)
	}
	if _, err := os.Stat(upgrade.LockPath(env.StateDir)); err == nil {
		t.Error("upgrade.lock taken by a shim that served no session")
	}
}

func TestFollowUpStartsAtFirstBind(t *testing.T) {
	t.Setenv(refresh.OffEnv, "")
	_, env, hs := fuSandbox(t, "", "claude")
	bound := make(chan struct{})
	stop, _ := startFollowUp(context.Background(), true, bound, env, hs, fuCfgDir(env), nil)
	defer stop()
	time.Sleep(100 * time.Millisecond)
	if fuDone(env, "claude") {
		t.Fatal("ran before a session bound")
	}
	close(bound)
	deadline := time.Now().Add(5 * time.Second)
	for !fuDone(env, "claude") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !fuDone(env, "claude") {
		t.Fatal("did not run after the first bind")
	}
}
