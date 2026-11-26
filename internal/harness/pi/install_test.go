package pi

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

const sbVersion = "9.9.9"

type sandbox struct {
	root, home, wd, bin string
	env                 harness.InstallEnv
	out                 *bytes.Buffer
	h                   *Harness
	shakes              []string
	shakeErr            error
}

// newSandbox points HOME, the agent dir, the working directory, the binary
// and PATH (a fake pi) under one temporary directory.
func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	root := t.TempDir()
	s := &sandbox{
		root: root, home: filepath.Join(root, "home"), wd: filepath.Join(root, "wd"),
		bin: filepath.Join(root, "bin", "crosstalk"), out: &bytes.Buffer{}, h: &Harness{},
	}
	write(t, s.bin, "#!/bin/sh\n", 0o755)
	if err := os.MkdirAll(s.wd, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", s.home)
	t.Setenv(AgentDirEnv, "")
	t.Setenv("PATH", filepath.Join(root, "path"))
	t.Chdir(s.wd)
	s.fakePi(t, "echo 1.6.0")
	s.env = harness.InstallEnv{
		Bin: s.bin, Version: sbVersion, Home: s.home, Out: s.out,
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		},
		Handshake: func(_ context.Context, command string, args, env []string) error {
			s.shakes = append(s.shakes, strings.Join(append(append([]string{command}, args...), env...), " "))
			return s.shakeErr
		},
	}
	s.guard(t)
	return s
}

// guard fails the test before anything runs against a path outside it.
func (s *sandbox) guard(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	realRoot, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		t.Fatal(err)
	}
	if realWD, err := filepath.EvalSymlinks(wd); err != nil || !strings.HasPrefix(realWD, realRoot+string(os.PathSeparator)) {
		t.Fatalf("working directory %s is outside the test directory %s", wd, s.root)
	}
	for _, p := range []string{AgentDir(s.env.Home), s.env.Home, s.env.Bin, os.Getenv("HOME")} {
		if !strings.HasPrefix(p, s.root+string(os.PathSeparator)) && !(!filepath.IsAbs(p) && p == AgentDir(s.env.Home)) {
			t.Fatalf("%s is outside the test directory %s", p, s.root)
		}
	}
}

func (s *sandbox) fakePi(t *testing.T, body string) {
	t.Helper()
	write(t, filepath.Join(s.root, "path", "pi"), "#!/bin/sh\n"+body+"\n", 0o755)
}

func (s *sandbox) agent() string { return AgentDir(s.home) }
func (s *sandbox) ext() string   { return extPath(s.agent()) }

func (s *sandbox) apply(t *testing.T, steps []harness.Step) error {
	t.Helper()
	s.guard(t)
	var errs []error
	for _, st := range steps {
		errs = append(errs, st.Apply(context.Background()))
	}
	return errors.Join(errs...)
}

func (s *sandbox) install(t *testing.T) error { return s.apply(t, s.h.InstallSteps(s.env)) }
func (s *sandbox) uninstall(t *testing.T) error {
	return s.apply(t, s.h.UninstallSteps(s.env))
}

func (s *sandbox) check(t *testing.T, id string) harness.CheckResult {
	t.Helper()
	s.guard(t)
	rs := s.h.Checks(context.Background(), s.env)
	i := slices.IndexFunc(rs, func(r harness.CheckResult) bool { return r.ID == id })
	if i < 0 {
		t.Fatalf("no %s in %+v", id, rs)
	}
	return rs[i]
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func absent(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

func TestPiInstallWritesOnce(t *testing.T) {
	s := newSandbox(t)
	if err := s.install(t); err != nil {
		t.Fatal(err)
	}
	want := string(Fill(s.bin, sbVersion, false, nil))
	if got := read(t, s.ext()); got != want {
		t.Fatalf("extension is not the filled embedded file:\n%s", got)
	}
	first, err := os.Stat(s.ext())
	if err != nil || first.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", first.Mode(), err)
	}
	if err := s.install(t); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(s.ext())
	if err != nil || read(t, s.ext()) != want || !os.SameFile(first, second) {
		t.Error("a second install rewrote the file")
	}
	if !absent(s.ext() + backupSuffix) {
		t.Error("a second install left a backup, so it rewrote the file")
	}
	if !absent(filepath.Join(s.agent(), "mcp.json")) {
		t.Error("install wrote an mcp.json entry")
	}
	if ok, err := s.h.Registered(s.env); !ok || err != nil {
		t.Errorf("Registered %v %v", ok, err)
	}
	if ok, err := s.h.SetUp(s.env); !ok || err != nil {
		t.Errorf("SetUp %v %v", ok, err)
	}
	other := s.env
	other.Bin = filepath.Join(s.root, "bin", "other")
	if ok, _ := s.h.Registered(other); ok {
		t.Error("Registered for another binary")
	}
}

func TestPiInstallNoAutoRefresh(t *testing.T) {
	s := newSandbox(t)
	s.env.NoAutoRefresh = true
	if err := s.install(t); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, s.ext()), refreshOff) {
		t.Fatal("--no-auto-refresh did not reach the file")
	}
	s.env.NoAutoRefresh = false
	if err := s.install(t); err != nil || !strings.Contains(read(t, s.ext()), refreshOff) {
		t.Errorf("a later install dropped the choice: %v", err)
	}
}

func TestPiInstallRefusesMCPEntry(t *testing.T) {
	for _, where := range []string{"global", "project"} {
		t.Run(where, func(t *testing.T) {
			s := newSandbox(t)
			path := filepath.Join(s.agent(), "mcp.json")
			if where == "project" {
				path = filepath.Join(s.wd, ".pi", "mcp.json")
			}
			write(t, path, `{"mcpServers":{"crosstalk":{"command":"x"}}}`, 0o600)
			err := s.install(t)
			if err == nil || !strings.Contains(err.Error(), "pi mcp remove crosstalk") {
				t.Fatalf("install: %v", err)
			}
			if strings.Contains(err.Error(), "--local") != (where == "project") {
				t.Errorf("wrong removal command: %v", err)
			}
			if !absent(filepath.Join(s.agent(), "extensions")) {
				t.Error("a refused install wrote a Pi file")
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		s := newSandbox(t)
		write(t, filepath.Join(s.agent(), "mcp.json"), "// not JSON", 0o600)
		if err := s.install(t); err == nil || !absent(s.ext()) {
			t.Fatalf("install over an unparseable mcp.json: %v", err)
		}
	})
	t.Run("other server", func(t *testing.T) {
		s := newSandbox(t)
		write(t, filepath.Join(s.agent(), "mcp.json"), `{"mcpServers":{"other":{}}}`, 0o600)
		if err := s.install(t); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPiForeignFileKept(t *testing.T) {
	s := newSandbox(t)
	const mine = "// my own extension\n// crosstalk-owned: written by `crosstalk install pi`\n"
	write(t, s.ext(), mine, 0o600)
	if err := s.install(t); err == nil {
		t.Error("install overwrote a foreign file without an error")
	}
	if err := s.uninstall(t); err != nil {
		t.Fatal(err)
	}
	if read(t, s.ext()) != mine || !absent(s.ext()+backupSuffix) {
		t.Error("a foreign file was changed")
	}
	if !strings.Contains(s.out.String(), "is kept") {
		t.Errorf("uninstall did not report the kept file: %q", s.out)
	}
	for name, f := range map[string]func(harness.InstallEnv) (bool, error){"Registered": s.h.Registered, "SetUp": s.h.SetUp} {
		if ok, err := f(s.env); ok || err != nil {
			t.Errorf("%s for a foreign file: %v %v", name, ok, err)
		}
	}
}

func TestPiForeignBackupAndLinkKept(t *testing.T) {
	t.Run("backup", func(t *testing.T) {
		s := newSandbox(t)
		write(t, s.ext(), string(Fill(s.bin, "0.0.1", false, nil)), 0o600)
		write(t, s.ext()+backupSuffix, "mine", 0o600)
		if err := s.install(t); err == nil || read(t, s.ext()+backupSuffix) != "mine" {
			t.Errorf("install overwrote a foreign backup: %v", err)
		}
		if err := s.uninstall(t); err != nil || read(t, s.ext()+backupSuffix) != "mine" {
			t.Errorf("uninstall removed a foreign backup: %v", err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		s := newSandbox(t)
		target := filepath.Join(s.root, "dev", "index.ts")
		write(t, target, string(Fill(s.bin, "0.0.1", false, nil)), 0o600)
		if err := os.MkdirAll(filepath.Dir(s.ext()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, s.ext()); err != nil {
			t.Fatal(err)
		}
		if err := s.install(t); err == nil {
			t.Error("install replaced a symlink")
		}
		if err := s.uninstall(t); err == nil || absent(s.ext()) {
			t.Errorf("uninstall of a linked extension that runs the binary: %v", err)
		}
	})
	t.Run("linked directory", func(t *testing.T) {
		s := newSandbox(t)
		dev := filepath.Join(s.root, "dev")
		old := string(Fill(s.bin, "0.0.1", false, nil))
		write(t, filepath.Join(dev, "index.ts"), old, 0o600)
		if err := os.MkdirAll(filepath.Dir(filepath.Dir(s.ext())), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dev, filepath.Dir(s.ext())); err != nil {
			t.Fatal(err)
		}
		if err := s.install(t); err == nil {
			t.Error("install wrote through a linked directory")
		}
		if err := s.uninstall(t); err == nil {
			t.Error("uninstall of a linked directory that runs the binary succeeded")
		}
		if read(t, filepath.Join(dev, "index.ts")) != old || !absent(filepath.Join(dev, "index.ts"+backupSuffix)) || absent(filepath.Dir(s.ext())) {
			t.Error("the linked directory or its target changed")
		}
	})
}

func TestPiShadowingFilesRefused(t *testing.T) {
	for _, name := range []string{"index.js", "package.json"} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			mine := filepath.Join(filepath.Dir(s.ext()), name)
			write(t, mine, "{}", 0o600)
			if err := s.install(t); err == nil || !absent(s.ext()) {
				t.Errorf("install beside %s: %v", name, err)
			}
		})
	}
	t.Run("package.json added later", func(t *testing.T) {
		s := installedSandbox(t)
		write(t, filepath.Join(filepath.Dir(s.ext()), "package.json"), "{}", 0o600)
		if r := s.check(t, "K2"); r.OK {
			t.Errorf("K2 with a package.json: %+v", r)
		}
	})
}

func TestPiLeftoverMCPEntry(t *testing.T) {
	s := newSandbox(t)
	if what, err := s.h.Leftover(s.env, true); what != "" || err != nil {
		t.Fatalf("nothing there: %q %v", what, err)
	}
	cfg := filepath.Join(s.agent(), "mcp.json")
	write(t, cfg, "\ufeff"+`{"mcpServers":{"crosstalk":{"command":"`+s.bin+`"}}}`, 0o600)
	if what, err := s.h.Leftover(s.env, false); what == "" || err != nil {
		t.Errorf("an entry running the binary: %q %v", what, err)
	}
	write(t, cfg, `{"mcpServers":{"crosstalk":{"command":"/other"}}}`, 0o600)
	if what, _ := s.h.Leftover(s.env, false); what != "" {
		t.Errorf("an entry running another command: %q", what)
	}
	if what, _ := s.h.Leftover(s.env, true); what == "" {
		t.Error("anyBin missed an entry")
	}
}

func TestPiAgentDirEnv(t *testing.T) {
	for name, c := range map[string]struct{ value, dir string }{
		"absolute": {"ROOT/profile", "profile"},
		"tilde":    {"~/profile", "home/profile"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			t.Setenv(AgentDirEnv, strings.Replace(c.value, "ROOT", s.root, 1))
			if err := s.install(t); err != nil {
				t.Fatal(err)
			}
			if absent(filepath.Join(s.root, c.dir, "extensions", "crosstalk", "index.ts")) {
				t.Error("no extension under $PI_CODING_AGENT_DIR")
			}
			if !absent(filepath.Join(s.home, ".pi")) {
				t.Error("wrote under ~/.pi with PI_CODING_AGENT_DIR set")
			}
		})
	}
	t.Run("relative", func(t *testing.T) {
		s := newSandbox(t)
		t.Setenv(AgentDirEnv, "profile")
		if err := s.install(t); err == nil || !absent(filepath.Join(s.wd, "profile")) {
			t.Errorf("install with a relative agent dir: %v", err)
		}
	})
}

func TestPiInstallVersions(t *testing.T) {
	old := versionTimeout
	versionTimeout = 500 * time.Millisecond
	t.Cleanup(func() { versionTimeout = old })
	for name, c := range map[string]struct {
		pi            string
		ok, wantsWarn bool
	}{
		"tested":   {"echo 1.6.0", true, false},
		"untested": {"echo 1.7.0", true, true},
		"2.x":      {"echo 2.0.0", false, false},
		"garbage":  {"echo nonsense", false, false},
		"fails":    {"exit 1", false, false},
		"hang":     {"exec /bin/sleep 30", false, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			s.fakePi(t, c.pi)
			start := time.Now()
			err := s.install(t)
			if c.ok != (err == nil) || c.ok == absent(s.ext()) {
				t.Fatalf("install: err %v, file absent %v", err, absent(s.ext()))
			}
			if got := strings.Contains(s.out.String(), "untested"); got != c.wantsWarn {
				t.Errorf("warning %v: %q", got, s.out)
			}
			if time.Since(start) > 10*time.Second {
				t.Error("the version probe ran past its deadline")
			}
		})
	}
}

func installedSandbox(t *testing.T) *sandbox {
	t.Helper()
	s := newSandbox(t)
	if err := s.install(t); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPiCheckK2(t *testing.T) {
	s := installedSandbox(t)
	if r := s.check(t, "K2"); !r.OK {
		t.Fatalf("K2: %+v", r)
	}
	for _, path := range []string{filepath.Join(s.agent(), "mcp.json"), filepath.Join(s.wd, ".pi", "mcp.json")} {
		write(t, path, `{"mcpServers":{"crosstalk":{}}}`, 0o600)
		if r := s.check(t, "K2"); r.OK || !strings.Contains(r.Detail, path[len(s.root):]) {
			t.Errorf("K2 with %s: %+v", path, r)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.uninstall(t); err != nil {
		t.Fatal(err)
	}
	if r := s.check(t, "K2"); r.OK {
		t.Errorf("K2 with no extension: %+v", r)
	}
}

func TestPiCheckK5(t *testing.T) {
	s := installedSandbox(t)
	if r := s.check(t, "K5"); !r.OK {
		t.Fatalf("K5: %+v", r)
	}
	other := filepath.Join(s.root, "bin", "other")
	write(t, other, "#!/bin/sh\n", 0o755)
	write(t, s.ext(), string(Fill(other, sbVersion, false, nil)), 0o600)
	if r := s.check(t, "K5"); r.OK {
		t.Errorf("K5 with another binary path: %+v", r)
	}
	write(t, s.ext(), string(Fill(s.bin, "0.0.1", false, nil)), 0o600)
	if r := s.check(t, "K5"); r.OK || !strings.Contains(r.Detail, "0.0.1") {
		t.Errorf("K5 with an old version: %+v", r)
	}
	if r := s.h.PluginCheck(context.Background(), s.env); r.ID != "K5" || r.OK {
		t.Errorf("PluginCheck: %+v", r)
	}
}

func TestPiCheckK6(t *testing.T) {
	s := installedSandbox(t)
	if r := s.check(t, "K6"); !r.OK || !slices.Equal(s.shakes, []string{s.bin + " mcp"}) {
		t.Fatalf("K6: %+v %q", r, s.shakes)
	}
	s.shakes, s.shakeErr = nil, errors.New("handshake broke")
	if r := s.check(t, "K6"); r.OK || !strings.Contains(r.Detail, "handshake broke") {
		t.Errorf("K6 with a failing handshake: %+v", r)
	}
	s.shakes, s.shakeErr = nil, nil
	write(t, s.ext(), string(Fill(s.bin, sbVersion, true, nil)), 0o600)
	if r := s.check(t, "K6"); !r.OK || !slices.Equal(s.shakes, []string{s.bin + " mcp " + harness.OffEnv + "=1"}) {
		t.Errorf("K6 env: %+v %q", r, s.shakes)
	}
	s.shakes = nil
	write(t, s.ext(), string(Fill(filepath.Join(s.root, "bin", "other"), sbVersion, false, nil)), 0o600)
	if r := s.check(t, "K6"); r.OK || len(s.shakes) != 0 {
		t.Errorf("K6 started another binary: %+v %q", r, s.shakes)
	}
}

func TestPiCheckK14(t *testing.T) {
	s := installedSandbox(t)
	settings := filepath.Join(s.agent(), "settings.json")
	write(t, settings, `{"extensions":["+builtin:mcp"]}`, 0o600)
	if r := s.check(t, "K14"); !r.OK || !r.WarnOnly || strings.Contains(r.Detail, AgentDirEnv) {
		t.Fatalf("K14: %+v", r)
	}
	write(t, settings, `{"extensions":["-builtin:mcp"]}`, 0o600)
	if r := s.check(t, "K14"); r.OK || !strings.Contains(r.Detail, "-builtin:mcp") {
		t.Errorf("K14 with builtin:mcp disabled: %+v", r)
	}
	write(t, settings, "{", 0o600)
	if r := s.check(t, "K14"); r.OK || !strings.Contains(r.Detail, "cannot read") {
		t.Errorf("K14 with broken settings: %+v", r)
	}
	if err := os.Remove(settings); err != nil {
		t.Fatal(err)
	}
	s.fakePi(t, "echo 1.7.0")
	if r := s.check(t, "K14"); r.OK || !strings.Contains(r.Detail, "untested") {
		t.Errorf("K14 with an untested minor: %+v", r)
	}
	s.fakePi(t, "echo 1.6.0")
	write(t, s.ext(), string(Fill(s.bin, "0.0.1", false, nil)), 0o600)
	if r := s.check(t, "K14"); r.OK || !strings.Contains(r.Detail, "0.0.1") {
		t.Errorf("K14 with an old extension: %+v", r)
	}
	old := versionTimeout
	versionTimeout = 500 * time.Millisecond
	t.Cleanup(func() { versionTimeout = old })
	s.fakePi(t, "exec /bin/sleep 30")
	if r := s.check(t, "K14"); r.OK || !strings.Contains(r.Detail, "pi --version") {
		t.Errorf("K14 with a hung pi: %+v", r)
	}
	s.fakePi(t, "echo 1.6.0")
	profile := filepath.Join(s.root, "profile")
	t.Setenv(AgentDirEnv, profile)
	if err := s.install(t); err != nil {
		t.Fatal(err)
	}
	if r := s.check(t, "K14"); !r.OK || !strings.Contains(r.Detail, profile+" (from $"+AgentDirEnv+")") {
		t.Errorf("K14 does not name the honoured agent dir: %+v", r)
	}
}

func TestPiUninstall(t *testing.T) {
	t.Run("after an upgrade, beside a user file", func(t *testing.T) {
		s := newSandbox(t)
		write(t, s.ext(), string(Fill(s.bin, "0.0.1", false, nil)), 0o600)
		if err := s.install(t); err != nil || absent(s.ext()+backupSuffix) {
			t.Fatalf("upgrade install: %v, backup absent %v", err, absent(s.ext()+backupSuffix))
		}
		mine := filepath.Join(filepath.Dir(s.ext()), "notes.txt")
		write(t, mine, "mine", 0o600)
		if err := s.uninstall(t); err != nil {
			t.Fatal(err)
		}
		if !absent(s.ext()) || !absent(s.ext()+backupSuffix) {
			t.Error("owned files left")
		}
		if read(t, mine) != "mine" || !strings.Contains(s.out.String(), "notes.txt") {
			t.Errorf("user file not kept and reported: %q", s.out)
		}
	})
	t.Run("empty directory", func(t *testing.T) {
		s := installedSandbox(t)
		if err := s.uninstall(t); err != nil {
			t.Fatal(err)
		}
		if !absent(filepath.Dir(s.ext())) || absent(filepath.Join(s.agent(), "extensions")) {
			t.Error("want only extensions/crosstalk removed")
		}
		if err := s.uninstall(t); err != nil {
			t.Errorf("second uninstall: %v", err)
		}
	})
	t.Run("nothing of ours", func(t *testing.T) {
		s := newSandbox(t)
		backup := s.ext() + backupSuffix
		write(t, backup, string(Fill(filepath.Join(s.root, "bin", "other"), "0.0.1", false, nil)), 0o600)
		if err := s.uninstall(t); err != nil || absent(backup) {
			t.Errorf("removed another binary's backup: %v", err)
		}
		if err := os.Remove(backup); err != nil {
			t.Fatal(err)
		}
		if err := s.uninstall(t); err != nil || absent(filepath.Dir(s.ext())) {
			t.Errorf("removed a directory it did not empty: %v", err)
		}
	})
	t.Run("another binary", func(t *testing.T) {
		s := installedSandbox(t)
		s.env.Bin = filepath.Join(s.root, "bin", "other")
		if err := s.uninstall(t); err != nil || absent(s.ext()) {
			t.Errorf("uninstall removed a file that runs another binary: %v", err)
		}
	})
}
