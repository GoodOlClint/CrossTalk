package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

const testBin = "/opt/crosstalk/bin/crosstalk"

const commentedConfig = `{
  // a user comment
  "$schema": "https://opencode.ai/config.json",
  "theme": "x", /* inline */
}
`

type fakeCLI struct {
	t         *testing.T
	cfgPath   string
	version   string
	ignoreEnv bool
	onAdd     func()
	failAdd   bool
	calls     []string
}

func (f *fakeCLI) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name != "opencode" {
		return nil, fmt.Errorf("unexpected command %s", name)
	}
	switch {
	case len(args) == 1 && args[0] == "--version":
		return []byte("opencode v" + f.version + "\n"), nil
	case len(args) >= 4 && args[0] == "mcp" && args[1] == "add" && args[2] == "crosstalk" && args[3] == "--global":
		if f.onAdd != nil {
			f.onAdd()
		}
		if f.failAdd {
			return nil, errors.New("add failed")
		}
		rest := args[4:]
		env := map[string]string{}
		for len(rest) >= 2 && rest[0] == "--env" {
			k, v, _ := strings.Cut(rest[1], "=")
			if !f.ignoreEnv {
				env[k] = v
			}
			rest = rest[2:]
		}
		if len(rest) < 2 || rest[0] != "--" {
			return nil, errors.New("bad args")
		}
		ent := map[string]any{"type": "local", "command": rest[1:]}
		if len(env) > 0 {
			ent["environment"] = env
		}
		b, _ := json.Marshal(map[string]any{"servers": map[string]any{"crosstalk": ent}})
		block := `"mcp": ` + string(b)
		cur, err := os.ReadFile(f.cfgPath)
		out := "{" + block + "}\n"
		if err == nil {
			s := string(cur)
			i := strings.LastIndex(s, "}")
			out = s[:i] + "  " + block + "\n" + s[i:]
		}
		if err := os.MkdirAll(filepath.Dir(f.cfgPath), 0o755); err != nil {
			return nil, err
		}
		return nil, os.WriteFile(f.cfgPath, []byte(out), 0o644)
	}
	return nil, fmt.Errorf("fake opencode: unhandled %v", args)
}

func (f *fakeCLI) adds() int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "opencode mcp add") {
			n++
		}
	}
	return n
}

type sandbox struct {
	home    string
	cfgPath string
	cli     *fakeCLI
	env     harness.InstallEnv
	h       *Harness
	svcDir  string
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	home := t.TempDir()
	cfgHome := filepath.Join(home, "xdg-config")
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("HOME", home)
	cfgPath := filepath.Join(cfgHome, "opencode", "opencode.json")
	cli := &fakeCLI{t: t, cfgPath: cfgPath, version: "2.4.1"}
	svcDir := t.TempDir()
	s := &sandbox{home: home, cfgPath: cfgPath, cli: cli, svcDir: svcDir}
	s.env = harness.InstallEnv{
		Bin: testBin, Version: "0.3.0", Home: home,
		Run:       cli.run,
		Handshake: func(context.Context, string, []string, []string) error { return nil },
	}
	s.h = NewHarness(Options{ServicePath: filepath.Join(svcDir, "service.json")})
	return s
}

func (s *sandbox) write(t *testing.T, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(s.cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (s *sandbox) read(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(s.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (s *sandbox) applyInstall(t *testing.T) error {
	t.Helper()
	steps := s.h.InstallSteps(s.env)
	if len(steps) == 0 {
		t.Fatal("no install steps")
	}
	for _, st := range steps {
		if err := st.Apply(context.Background()); err != nil {
			return err
		}
	}
	return nil
}

func (s *sandbox) applyUninstall(t *testing.T) error {
	t.Helper()
	for _, st := range s.h.UninstallSteps(s.env) {
		if err := st.Apply(context.Background()); err != nil {
			return err
		}
	}
	return nil
}

func (s *sandbox) noBackup(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(s.cfgPath + ".crosstalk-backup"); err == nil {
		t.Errorf("a backup was written for a run that wrote nothing")
	}
}

func TestInstallKeepsCommentsAndIsIdempotent(t *testing.T) {
	s := newSandbox(t)
	s.write(t, commentedConfig)
	if err := s.applyInstall(t); err != nil {
		t.Fatal(err)
	}
	got := s.read(t)
	for _, c := range []string{"// a user comment", "/* inline */"} {
		if !strings.Contains(got, c) {
			t.Errorf("comment %q lost:\n%s", c, got)
		}
	}
	if !strings.Contains(got, testBin) {
		t.Errorf("entry missing:\n%s", got)
	}
	bak, err := os.ReadFile(s.cfgPath + ".crosstalk-backup")
	if err != nil || string(bak) != commentedConfig {
		t.Errorf("backup = %q, %v; want the original bytes", bak, err)
	}
	if ok, err := s.h.Registered(s.env); err != nil || !ok {
		t.Errorf("Registered = %v, %v", ok, err)
	}
	if err := s.applyInstall(t); err != nil {
		t.Fatal(err)
	}
	if s.read(t) != got {
		t.Errorf("second install changed the file:\n%s", s.read(t))
	}
	if n := s.cli.adds(); n != 1 {
		t.Errorf("mcp add ran %d times, want 1", n)
	}
}

func TestInstallCarriesNoAutoRefresh(t *testing.T) {
	s := newSandbox(t)
	s.env.NoAutoRefresh = true
	if err := s.applyInstall(t); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.read(t), harness.OffEnv) {
		t.Errorf("environment missing %s:\n%s", harness.OffEnv, s.read(t))
	}
	before := s.read(t)
	if err := s.applyInstall(t); err != nil || s.read(t) != before || s.cli.adds() != 1 {
		t.Errorf("rerun: err %v, adds %d, changed %v", err, s.cli.adds(), s.read(t) != before)
	}
}

func TestInstallConflictingEntryAbortsWithNoWrites(t *testing.T) {
	for name, body := range map[string]string{
		"v2": `{"mcp":{"servers":{"crosstalk":{"type":"local","command":["/other/bin/crosstalk","mcp"]}}}}` + "\n",
		"v1": `{"mcp":{"crosstalk":{"type":"local","command":["/other/bin/crosstalk","mcp"]}}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			s.write(t, body)
			err := s.applyInstall(t)
			if err == nil || !strings.Contains(err.Error(), "/other/bin/crosstalk") || !strings.Contains(err.Error(), s.cfgPath) {
				t.Fatalf("err = %v, want a conflict naming the command and the file", err)
			}
			if s.read(t) != body {
				t.Errorf("file changed:\n%s", s.read(t))
			}
			if s.cli.adds() != 0 {
				t.Errorf("mcp add ran")
			}
			s.noBackup(t)
		})
	}
}

func TestInstallRefusesUnreadableConfigBeforeWriting(t *testing.T) {
	s := newSandbox(t)
	body := `{"mcp": [1, 2`
	s.write(t, body)
	if err := s.applyInstall(t); err == nil {
		t.Fatal("install accepted a config it cannot parse")
	}
	if s.read(t) != body || s.cli.adds() != 0 {
		t.Errorf("wrote despite the parse error")
	}
	s.write(t, `{"mcp":{"servers":"nope"}}`)
	if err := s.applyInstall(t); err == nil || s.cli.adds() != 0 {
		t.Errorf("a non-object mcp.servers was accepted: %v", err)
	}
}

func TestInstallRestoresFileWhenEnvironmentEditFails(t *testing.T) {
	s := newSandbox(t)
	s.env.NoAutoRefresh = true
	s.cli.ignoreEnv = true
	s.write(t, commentedConfig)
	err := s.applyInstall(t)
	if err == nil || !strings.Contains(err.Error(), harness.OffEnv) {
		t.Fatalf("err = %v, want a failure naming %s", err, harness.OffEnv)
	}
	if s.read(t) != commentedConfig {
		t.Errorf("original bytes not restored:\n%s", s.read(t))
	}

	s2 := newSandbox(t)
	s2.env.NoAutoRefresh = true
	s2.cli.ignoreEnv = true
	if err := s2.applyInstall(t); err == nil {
		t.Fatal("want failure")
	}
	if _, err := os.Stat(s2.cfgPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a config that did not exist before was left behind: %v", err)
	}
}

func TestInstallRefusesV1Service(t *testing.T) {
	s := newSandbox(t)
	s.cli.version = "1.14.2"
	s.write(t, commentedConfig)
	err := s.applyInstall(t)
	if err == nil || !strings.Contains(err.Error(), "OpenCode 1.x found: unsupported, see ADR 0032") {
		t.Fatalf("err = %v", err)
	}
	if s.read(t) != commentedConfig || s.cli.adds() != 0 {
		t.Errorf("wrote despite a v1 binary")
	}
	s.noBackup(t)
}

func TestUninstallRemovesOnlyOwnEntry(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"v2": {
			body: "{\n  \"mcp\": {\n    \"servers\": {\n      \"other\": {\"type\": \"local\", \"command\": [\"/bin/other\"]},\n      \"crosstalk\": {\n        \"type\": \"local\",\n        \"command\": [\"" + testBin + "\", \"mcp\"]\n      }\n    }\n  },\n  \"theme\": \"x\"\n}\n",
			want: "other",
		},
		"v1": {
			body: "{\n  \"mcp\": {\n    \"crosstalk\": {\"type\": \"local\", \"command\": [\"" + testBin + "\", \"mcp\"]},\n    \"other\": {\"type\": \"local\", \"command\": [\"/bin/other\"]}\n  }\n}\n",
			want: "other",
		},
		"only": {
			body: `{"mcp":{"servers":{"crosstalk":{"type":"local","command":["` + testBin + `","mcp"]}}}}` + "\n",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			s.write(t, c.body)
			if err := s.applyUninstall(t); err != nil {
				t.Fatal(err)
			}
			got := s.read(t)
			if strings.Contains(got, "crosstalk") {
				t.Errorf("entry still there:\n%s", got)
			}
			var v map[string]any
			if err := json.Unmarshal([]byte(got), &v); err != nil {
				t.Fatalf("result is not JSON: %v\n%s", err, got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Errorf("lost %q:\n%s", c.want, got)
			}
			if err := s.applyUninstall(t); err != nil || s.read(t) != got {
				t.Errorf("second uninstall: %v", err)
			}
		})
	}

	t.Run("other command stays", func(t *testing.T) {
		s := newSandbox(t)
		body := `{"mcp":{"servers":{"crosstalk":{"type":"local","command":["/other/bin/crosstalk","mcp"]}}}}` + "\n"
		s.write(t, body)
		if err := s.applyUninstall(t); err != nil || s.read(t) != body {
			t.Errorf("removed an entry that runs another binary: %v\n%s", err, s.read(t))
		}
	})
	t.Run("no file", func(t *testing.T) {
		s := newSandbox(t)
		if err := s.applyUninstall(t); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUninstallRefusesCommentedConfig(t *testing.T) {
	s := newSandbox(t)
	body := "{\n  // keep me\n  \"mcp\": {\n    \"servers\": {\n      \"crosstalk\": {\"type\": \"local\", \"command\": [\"" + testBin + "\", \"mcp\"]},\n    }\n  }\n}\n"
	s.write(t, body)
	err := s.applyUninstall(t)
	if err == nil {
		t.Fatal("uninstall rewrote a commented config")
	}
	for _, want := range []string{s.cfgPath, `"crosstalk": {"type": "local"`, "comments"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if s.read(t) != body {
		t.Errorf("file changed")
	}
	s.noBackup(t)
}

func TestRegisteredAndSetUpReadConfigOnly(t *testing.T) {
	s := newSandbox(t)
	s.env.Run = func(context.Context, string, ...string) ([]byte, error) {
		t.Error("Registered or SetUp ran a command")
		return nil, errors.New("no")
	}
	if ok, err := s.h.Registered(s.env); ok || err != nil {
		t.Errorf("no file: Registered = %v, %v", ok, err)
	}
	if ok, err := s.h.SetUp(s.env); ok || err != nil {
		t.Errorf("no file: SetUp = %v, %v", ok, err)
	}
	s.write(t, "{\n // c\n \"mcp\": {\"servers\": {\"crosstalk\": {\"type\": \"local\", \"command\": [\"/other/crosstalk\", \"mcp\"]},},},\n}\n")
	if ok, err := s.h.Registered(s.env); ok || err != nil {
		t.Errorf("other binary: Registered = %v, %v", ok, err)
	}
	if ok, err := s.h.SetUp(s.env); !ok || err != nil {
		t.Errorf("other binary: SetUp = %v, %v", ok, err)
	}
	s.write(t, `{"mcp":{"crosstalk":{"type":"local","command":["`+testBin+`","mcp"]}}}`)
	if ok, err := s.h.Registered(s.env); !ok || err != nil {
		t.Errorf("v1 shape: Registered = %v, %v", ok, err)
	}
	s.write(t, `{"mcp":`)
	if ok, err := s.h.SetUp(s.env); !ok || err == nil {
		t.Errorf("unreadable: SetUp = %v, %v; want true with an error", ok, err)
	}
	if _, err := s.h.Registered(s.env); err == nil {
		t.Errorf("unreadable: Registered gave no error")
	}
}

func checkByID(rs []harness.CheckResult, id string) (harness.CheckResult, bool) {
	for _, r := range rs {
		if r.ID == id {
			return r, true
		}
	}
	return harness.CheckResult{}, false
}

func entryJSON(bin string) string {
	return `"type":"local","command":["` + bin + `","mcp"]`
}

func TestChecksK2K5K6K13(t *testing.T) {
	ctx := context.Background()
	project := func(t *testing.T, body string) {
		t.Helper()
		dir := t.TempDir()
		t.Chdir(dir)
		if body != "" {
			if err := os.WriteFile(filepath.Join(dir, "opencode.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	setBin := func(t *testing.T, s *sandbox) {
		bin := filepath.Join(t.TempDir(), "crosstalk")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		s.env.Bin = bin
	}
	cfg := func(s *sandbox) string {
		return `{"mcp":{"servers":{"crosstalk":{` + entryJSON(s.env.Bin) + `}}}}`
	}

	t.Run("ids", func(t *testing.T) {
		s := newSandbox(t)
		setBin(t, s)
		project(t, "")
		s.write(t, cfg(s))
		var ids []string
		for _, r := range s.h.Checks(ctx, s.env) {
			ids = append(ids, r.ID)
			if !r.OK {
				t.Errorf("%s fails on a clean install: %s", r.ID, r.Detail)
			}
		}
		if !slices.Equal(ids, []string{"K2", "K5", "K6", "K13"}) {
			t.Errorf("ids = %v", ids)
		}
	})

	t.Run("K2 both shapes", func(t *testing.T) {
		s := newSandbox(t)
		setBin(t, s)
		project(t, "")
		s.write(t, `{"mcp":{"crosstalk":{`+entryJSON(s.env.Bin)+`},"servers":{"crosstalk":{`+entryJSON(s.env.Bin)+`}}}}`)
		r, _ := checkByID(s.h.Checks(ctx, s.env), "K2")
		if r.OK || !strings.Contains(r.Detail, "2") {
			t.Errorf("K2 = %+v", r)
		}
	})

	t.Run("K2 project duplicate", func(t *testing.T) {
		s := newSandbox(t)
		setBin(t, s)
		project(t, `{"mcp":{"servers":{"crosstalk":{`+entryJSON("/elsewhere/crosstalk")+`}}}}`)
		s.write(t, cfg(s))
		r, _ := checkByID(s.h.Checks(ctx, s.env), "K2")
		if r.OK || !strings.Contains(r.Detail, "opencode.json") {
			t.Errorf("K2 = %+v", r)
		}
	})

	t.Run("K2 none", func(t *testing.T) {
		s := newSandbox(t)
		project(t, "")
		r, _ := checkByID(s.h.Checks(ctx, s.env), "K2")
		if r.OK {
			t.Errorf("K2 passes with no entry: %+v", r)
		}
	})

	t.Run("K5 wrong binary", func(t *testing.T) {
		s := newSandbox(t)
		setBin(t, s)
		project(t, "")
		s.write(t, `{"mcp":{"servers":{"crosstalk":{`+entryJSON("/elsewhere/crosstalk")+`}}}}`)
		rs := s.h.Checks(ctx, s.env)
		k5, _ := checkByID(rs, "K5")
		if k5.OK || !strings.Contains(k5.Detail, "/elsewhere/crosstalk") {
			t.Errorf("K5 = %+v", k5)
		}
	})

	t.Run("K6 handshake", func(t *testing.T) {
		s := newSandbox(t)
		setBin(t, s)
		project(t, "")
		s.write(t, `{"mcp":{"servers":{"crosstalk":{`+entryJSON(s.env.Bin)+`,"environment":{"CROSSTALK_NO_AUTO_REFRESH":"1"}}}}}`)
		var gotCmd string
		var gotEnv []string
		s.env.Handshake = func(_ context.Context, c string, _ []string, e []string) error {
			gotCmd, gotEnv = c, e
			return errors.New("no tools listed")
		}
		k6, _ := checkByID(s.h.Checks(ctx, s.env), "K6")
		if k6.OK || !strings.Contains(k6.Detail, "no tools listed") {
			t.Errorf("K6 = %+v", k6)
		}
		if gotCmd != s.env.Bin || !slices.Equal(gotEnv, []string{"CROSSTALK_NO_AUTO_REFRESH=1"}) {
			t.Errorf("handshake got %q %v", gotCmd, gotEnv)
		}
	})

	k13 := func(t *testing.T, mode os.FileMode, url, version string) harness.CheckResult {
		t.Helper()
		s := newSandbox(t)
		setBin(t, s)
		project(t, "")
		s.write(t, cfg(s))
		if url != "" {
			p := writeSvc(t, s.svcDir, url, version, 4242)
			if err := os.Chmod(p, mode); err != nil {
				t.Fatal(err)
			}
		}
		r, ok := checkByID(s.h.Checks(ctx, s.env), "K13")
		if !ok {
			t.Fatal("no K13")
		}
		if !r.WarnOnly {
			t.Errorf("K13 must be warn-only: %+v", r)
		}
		return r
	}
	t.Run("K13", func(t *testing.T) {
		if r := k13(t, 0o600, "http://127.0.0.1:49374", "2.4.1"); !r.OK {
			t.Errorf("tested version fails: %+v", r)
		}
		if r := k13(t, 0o644, "http://127.0.0.1:49374", "2.4.1"); r.OK || !strings.Contains(r.Detail, "0600") {
			t.Errorf("mode 0644 = %+v", r)
		}
		if r := k13(t, 0o600, "http://192.0.2.7:49374", "2.4.1"); r.OK || !strings.Contains(r.Detail, "loopback") {
			t.Errorf("non-loopback = %+v", r)
		}
		if r := k13(t, 0o600, "http://127.0.0.1:49374", "1.14.2"); r.OK || !strings.Contains(r.Detail, "ADR 0032") || !strings.Contains(r.Detail, "1.x") {
			t.Errorf("1.x = %+v", r)
		}
		if r := k13(t, 0o600, "http://127.0.0.1:49374", "2.9.0"); r.OK || !regexp.MustCompile(`untested`).MatchString(r.Detail) {
			t.Errorf("2.9 = %+v", r)
		}
		if r := k13(t, 0, "", ""); !r.OK || !strings.Contains(r.Detail, "not running") {
			t.Errorf("missing service.json = %+v", r)
		}
	})
}

func TestInstallRefusesToReAddExistingEntryForEnvironment(t *testing.T) {
	s := newSandbox(t)
	s.env.NoAutoRefresh = true
	body := `{"mcp":{"servers":{"crosstalk":{` + entryJSON(testBin) + `,"environment":{"MY_VAR":"1"},"timeout":9}}}}` + "\n"
	s.write(t, body)
	err := s.applyInstall(t)
	if err == nil || !strings.Contains(err.Error(), harness.OffEnv) {
		t.Fatalf("err = %v", err)
	}
	if s.read(t) != body || s.cli.adds() != 0 {
		t.Errorf("an existing entry was rewritten:\n%s", s.read(t))
	}
}

func TestInstallRollbackLeavesSiblingFile(t *testing.T) {
	s := newSandbox(t)
	sibling := filepath.Join(filepath.Dir(s.cfgPath), "opencode.jsonc")
	s.cli.failAdd = true
	s.cli.onAdd = func() {
		os.MkdirAll(filepath.Dir(sibling), 0o755)
		os.WriteFile(sibling, []byte("{}\n"), 0o644)
	}
	if err := s.applyInstall(t); err == nil {
		t.Fatal("want failure")
	}
	if b, err := os.ReadFile(sibling); err != nil || string(b) != "{}\n" {
		t.Errorf("rollback touched the sibling file: %q, %v", b, err)
	}
}

func TestInstallAndUninstallRefuseSymlinkedConfig(t *testing.T) {
	s := newSandbox(t)
	real := filepath.Join(t.TempDir(), "dotfiles.json")
	body := `{"mcp":{"servers":{"crosstalk":{` + entryJSON(testBin) + `}}}}` + "\n"
	os.WriteFile(real, []byte(body), 0o644)
	os.MkdirAll(filepath.Dir(s.cfgPath), 0o755)
	if err := os.Symlink(real, s.cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := s.applyUninstall(t); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("uninstall err = %v", err)
	}
	os.Remove(s.cfgPath)
	os.WriteFile(real, []byte("{}\n"), 0o644)
	os.Symlink(real, s.cfgPath)
	if err := s.applyInstall(t); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("install err = %v", err)
	}
	if fi, err := os.Lstat(s.cfgPath); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced")
	}
}

func TestChecksEmptyCommandAndDisabledEntry(t *testing.T) {
	ctx := context.Background()
	s := newSandbox(t)
	t.Chdir(t.TempDir())
	s.write(t, `{"mcp":{"servers":{"crosstalk":{"type":"local","command":[]}}}}`)
	rs := s.h.Checks(ctx, s.env)
	if k2, _ := checkByID(rs, "K2"); k2.OK {
		t.Errorf("K2 passes with an empty command: %+v", k2)
	}
	s.write(t, `{"mcp":{"servers":{"crosstalk":{`+entryJSON(testBin)+`,"enabled":false}}}}`)
	if k2, _ := checkByID(s.h.Checks(ctx, s.env), "K2"); k2.OK || !strings.Contains(k2.Detail, "disabled") {
		t.Errorf("K2 on a disabled entry: %+v", k2)
	}
	if err := s.applyInstall(t); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("install over a disabled entry: %v", err)
	}
}

func TestK6NeverStartsProjectOrMismatchedCommand(t *testing.T) {
	ctx := context.Background()
	s := newSandbox(t)
	started := false
	s.env.Handshake = func(context.Context, string, []string, []string) error { started = true; return nil }
	proj := t.TempDir()
	t.Chdir(proj)
	os.WriteFile(filepath.Join(proj, "opencode.json"), []byte(`{"mcp":{"servers":{"crosstalk":{`+entryJSON("/repo/evil")+`}}}}`), 0o644)
	rs := s.h.Checks(ctx, s.env)
	if started {
		t.Error("handshake started a project-only entry")
	}
	if k6, _ := checkByID(rs, "K6"); k6.OK {
		t.Errorf("K6 passes: %+v", k6)
	}
	s.write(t, `{"mcp":{"servers":{"crosstalk":{`+entryJSON("/other/bin/crosstalk")+`}}}}`)
	s.h.Checks(ctx, s.env)
	if started {
		t.Error("handshake started a command K5 rejected")
	}
}
