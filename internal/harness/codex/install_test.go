package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

type installRecorder struct {
	calls []string
	path  string
	fail  string
}

func (r *installRecorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	c := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, c)
	if c == r.fail {
		return nil, errors.New("forced failure")
	}
	if c == "codex mcp get crosstalk --json" {
		b, _ := os.ReadFile(r.path)
		e, ok := readEntry(string(b))
		if !ok {
			return nil, errors.New("no server")
		}
		var out registration
		out.Transport.Command, out.Transport.Args = e.command, e.args
		return json.Marshal(out)
	}
	return nil, nil
}

func codexEnv(t *testing.T, fixture string) (harness.InstallEnv, *installRecorder) {
	t.Helper()
	home := t.TempDir()
	ch := filepath.Join(home, ".codex")
	t.Setenv("CODEX_HOME", ch)
	r := &installRecorder{path: filepath.Join(ch, "config.toml")}
	if fixture != "" {
		b, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(ch, 0o700)
		if err := os.WriteFile(r.path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return harness.InstallEnv{Bin: "/opt/x/crosstalk", Version: "9.9.9", Home: home, Run: r.run}, r
}

func applySteps(steps []harness.Step) error {
	for _, s := range steps {
		if err := s.Apply(context.Background()); err != nil {
			return err
		}
	}
	return nil
}

func TestCodexConfigEdit(t *testing.T) {
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	fixture := func(f string) string { return read(filepath.Join("testdata", f)) }
	const set = "command = \"/opt/x/crosstalk\"\nargs = [\"mcp\"]\n"

	t.Run("no table appends one with approve", func(t *testing.T) {
		env, r := codexEnv(t, "config-none.toml")
		if err := applySteps((&Harness{}).InstallSteps(env)); err != nil {
			t.Fatal(err)
		}
		want := fixture("config-none.toml") + "\n[mcp_servers.crosstalk]\n" + set + "default_tools_approval_mode = \"approve\"\n"
		if got := read(r.path); got != want {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
		if pa, ml := slices.Index(r.calls, "codex plugin add crosstalk@crosstalk"), slices.Index(r.calls, "codex mcp list"); pa < 0 || ml < pa {
			t.Errorf("plugin add must precede the config edit: %v", r.calls)
		}
	})

	t.Run("existing table keeps approval mode and tools", func(t *testing.T) {
		env, r := codexEnv(t, "config-prompt.toml")
		if err := applySteps((&Harness{}).InstallSteps(env)); err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(fixture("config-prompt.toml"), "command = \"crosstalk\"", "command = \"/opt/x/crosstalk\"", 1)
		if got := read(r.path); got != want {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
		if b, _ := os.ReadFile(r.path + ".crosstalk-backup"); string(b) != fixture("config-prompt.toml") {
			t.Errorf("backup %q", b)
		}
	})

	t.Run("inline table refused", func(t *testing.T) {
		env, r := codexEnv(t, "config-inline.toml")
		err := applySteps((&Harness{}).InstallSteps(env))
		if err == nil || !strings.Contains(err.Error(), "[mcp_servers.crosstalk]") {
			t.Errorf("want a refusal with the manual text, got %v", err)
		}
		if read(r.path) != fixture("config-inline.toml") {
			t.Error("config changed")
		}
		if slices.Contains(r.calls, "codex mcp list") {
			t.Error("verified an edit that was refused")
		}
	})

	t.Run("orphan tools table", func(t *testing.T) {
		if !orphanTables(fixture("config-orphan.toml")) {
			t.Error("orphan not found")
		}
		for _, f := range []string{"config-none.toml", "config-prompt.toml", "config-wrapper.toml"} {
			if orphanTables(fixture(f)) {
				t.Errorf("%s: false orphan", f)
			}
		}
		env, r := codexEnv(t, "config-orphan.toml")
		if err := applySteps((&Harness{}).InstallSteps(env)); err != nil {
			t.Fatal(err)
		}
		if orphanTables(read(r.path)) {
			t.Error("install left the orphan")
		}
	})

	t.Run("wrapper entry gets args mcp", func(t *testing.T) {
		env, r := codexEnv(t, "config-wrapper.toml")
		steps := (&Harness{}).InstallSteps(env)
		if !strings.Contains(steps[len(steps)-1].Change, "/usr/bin/env") {
			t.Errorf("wrapper not shown: %q", steps[len(steps)-1].Change)
		}
		if err := applySteps(steps); err != nil {
			t.Fatal(err)
		}
		got := read(r.path)
		e, _ := readEntry(got)
		if e.command != "/opt/x/crosstalk" || !slices.Equal(e.args, []string{"mcp"}) {
			t.Errorf("entry %+v", e)
		}
		if !strings.Contains(got, "[mcp_servers.crosstalk.env]\nCROSSTALK_CODEX_ENDPOINTS = \"unix:///s.sock\"\n") {
			t.Errorf("env table lost:\n%s", got)
		}
	})

	t.Run("failed verification restores the backup", func(t *testing.T) {
		env, r := codexEnv(t, "config-prompt.toml")
		r.fail = "codex mcp list"
		if err := applySteps((&Harness{}).InstallSteps(env)); err == nil {
			t.Fatal("want an error")
		}
		if read(r.path) != fixture("config-prompt.toml") {
			t.Errorf("not restored:\n%s", read(r.path))
		}
	})
}
