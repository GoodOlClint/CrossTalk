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
			rec.claudeVer = "0.0.1"
		case "codex":
			p := filepath.Join(home, ".codex", "config.toml")
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte("[mcp_servers.crosstalk]\ncommand = \"/x/crosstalk\"\nargs = [\"mcp\"]\n"), 0o644)
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
	os.WriteFile(files[1], []byte(`{"mcpServers":{}}`), 0o644)
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
	if startFollowUp(ctx, false, env, hs, fuCfgDir(env), nil) {
		t.Error("started with refresh not armed")
	}
	t.Setenv(refresh.OffEnv, "1")
	if startFollowUp(ctx, true, env, hs, fuCfgDir(env), nil) {
		t.Errorf("started with %s=1", refresh.OffEnv)
	}
	time.Sleep(100 * time.Millisecond)
	if c := rec.all(); len(c) != 0 {
		t.Fatalf("commands ran while off: %v", c)
	}
	t.Setenv(refresh.OffEnv, "0")
	if !startFollowUp(ctx, true, env, hs, fuCfgDir(env), nil) {
		t.Fatal("did not start when armed")
	}
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
