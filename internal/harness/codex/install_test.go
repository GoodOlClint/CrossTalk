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
	dropEnv bool
	calls   []string
	path    string
	fail    string
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
		if !r.dropEnv && strings.Contains(string(b), "CROSSTALK_NO_AUTO_REFRESH = \"1\"") {
			out.Transport.Env = map[string]string{"CROSSTALK_NO_AUTO_REFRESH": "1"}
		}
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
	return harness.InstallEnv{Bin: "/opt/x/crosstalk", Version: "9.9.9", Home: home, Run: r.run, Marketplace: "GoodOlClint/CrossTalk"}, r
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

	t.Run("brackets inside strings and comments", func(t *testing.T) {
		env, r := codexEnv(t, "config-quoted.toml")
		if err := applySteps((&Harness{}).InstallSteps(env)); err != nil {
			t.Fatal(err)
		}
		want := "[mcp_servers.crosstalk]\n" + set + "default_tools_approval_mode = \"prompt\"\n\n[mcp_servers.crosstalk.env]\nCROSSTALK_CODEX_ENDPOINTS = \"unix:///s.sock\"\n"
		if got := read(r.path); got != want {
			t.Errorf("got\n%s\nwant\n%s", got, want)
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

func TestCodexLocalMarketplaceSkipsUpgrade(t *testing.T) {
	env, r := codexEnv(t, "config-none.toml")
	env.Marketplace = "/scratch/mkt"
	if err := applySteps((&Harness{}).InstallSteps(env)); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.calls, "codex plugin marketplace add /scratch/mkt") {
		t.Errorf("marketplace not added: %v", r.calls)
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "codex plugin marketplace upgrade") {
			t.Errorf("upgrade run for a local marketplace, which codex refuses: %v", r.calls)
		}
	}
}

func TestInstallNoAutoRefreshWritesEnv(t *testing.T) {
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	fixture := func(f string) string { return read(filepath.Join("testdata", f)) }
	const kv = "CROSSTALK_NO_AUTO_REFRESH = \"1\"\n"
	run := func(t *testing.T, fx string, flag bool) (string, string) {
		env, r := codexEnv(t, fx)
		env.NoAutoRefresh = flag
		steps := (&Harness{}).InstallSteps(env)
		if flag && !strings.Contains(steps[len(steps)-1].Change, "CROSSTALK_NO_AUTO_REFRESH") {
			t.Errorf("dry-run text lacks the env: %q", steps[len(steps)-1].Change)
		}
		if err := applySteps(steps); err != nil {
			t.Fatal(err)
		}
		return read(r.path), strings.Join(r.calls, "\n")
	}

	t.Run("fresh table", func(t *testing.T) {
		got, _ := run(t, "config-none.toml", true)
		want := fixture("config-none.toml") + "\n[mcp_servers.crosstalk]\ncommand = \"/opt/x/crosstalk\"\nargs = [\"mcp\"]\ndefault_tools_approval_mode = \"approve\"\n\n[mcp_servers.crosstalk.env]\n" + kv
		if got != want {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("existing table keeps its other keys and sub-tables", func(t *testing.T) {
		got, _ := run(t, "config-prompt.toml", true)
		base := strings.Replace(fixture("config-prompt.toml"), "command = \"crosstalk\"", "command = \"/opt/x/crosstalk\"", 1)
		hdr := "default_tools_approval_mode = \"prompt\"\n"
		want := strings.Replace(base, hdr, hdr+"\n[mcp_servers.crosstalk.env]\n"+kv, 1)
		if got != want {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("re-run is idempotent and replaces a stale value", func(t *testing.T) {
		env, r := codexEnv(t, "config-prompt.toml")
		env.NoAutoRefresh = true
		if err := applySteps((&Harness{}).InstallSteps(env)); err != nil {
			t.Fatal(err)
		}
		once := read(r.path)
		if err := applySteps((&Harness{}).InstallSteps(env)); err != nil {
			t.Fatal(err)
		}
		if twice := read(r.path); twice != once {
			t.Errorf("second run changed the file:\n%s\nvs\n%s", twice, once)
		}
		stale := strings.Replace(once, kv, "OTHER = \"x\"\nCROSSTALK_NO_AUTO_REFRESH = \"0\"\n", 1)
		os.WriteFile(r.path, []byte(stale), 0o600)
		if err := applySteps((&Harness{}).InstallSteps(env)); err != nil {
			t.Fatal(err)
		}
		if got, want := read(r.path), strings.Replace(once, kv, "OTHER = \"x\"\n"+kv, 1); got != want {
			t.Errorf("got\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("without the flag an existing entry stays", func(t *testing.T) {
		env, r := codexEnv(t, "config-prompt.toml")
		env.NoAutoRefresh = true
		if err := applySteps((&Harness{}).InstallSteps(env)); err != nil {
			t.Fatal(err)
		}
		withEnv := read(r.path)
		env.NoAutoRefresh = false
		if err := applySteps((&Harness{}).InstallSteps(env)); err != nil {
			t.Fatal(err)
		}
		if got := read(r.path); got != withEnv {
			t.Errorf("got\n%s\nwant\n%s", got, withEnv)
		}
	})
	t.Run("without the flag nothing is written", func(t *testing.T) {
		got, _ := run(t, "config-none.toml", false)
		if strings.Contains(got, "NO_AUTO_REFRESH") {
			t.Errorf("unexpected env:\n%s", got)
		}
	})
	t.Run("inline env key is refused", func(t *testing.T) {
		env, r := codexEnv(t, "")
		os.MkdirAll(filepath.Dir(r.path), 0o700)
		orig := "[mcp_servers.crosstalk]\ncommand = \"crosstalk\"\nargs = [\"mcp\"]\nenv = { A = \"1\" }\n"
		os.WriteFile(r.path, []byte(orig), 0o600)
		env.NoAutoRefresh = true
		if err := applySteps((&Harness{}).InstallSteps(env)); err == nil || !strings.Contains(err.Error(), "env") {
			t.Errorf("want a refusal naming env, got %v", err)
		}
		if read(r.path) != orig {
			t.Error("config changed")
		}
	})
	t.Run("dotted env key is refused", func(t *testing.T) {
		env, r := codexEnv(t, "")
		os.MkdirAll(filepath.Dir(r.path), 0o700)
		orig := "[mcp_servers.crosstalk]\ncommand = \"crosstalk\"\nargs = [\"mcp\"]\nenv.KEEP = \"x\"\n"
		os.WriteFile(r.path, []byte(orig), 0o600)
		env.NoAutoRefresh = true
		if err := applySteps((&Harness{}).InstallSteps(env)); err == nil {
			t.Error("want a refusal")
		}
		if read(r.path) != orig {
			t.Error("config changed")
		}
	})
	t.Run("codex not reading the env rolls the edit back", func(t *testing.T) {
		env, r := codexEnv(t, "config-prompt.toml")
		r.dropEnv = true
		env.NoAutoRefresh = true
		if err := applySteps((&Harness{}).InstallSteps(env)); err == nil || !strings.Contains(err.Error(), "NO_AUTO_REFRESH") {
			t.Errorf("want a rollback naming the env, got %v", err)
		}
		if read(r.path) != fixture("config-prompt.toml") {
			t.Error("config not restored")
		}
	})
}

const differentSource = "Error: marketplace 'crosstalk' is already added from a different source; remove it before adding this source"

// marketplaceFake is codex whose add fails as on another source until an
// upgrade succeeds; upgradeErr makes the upgrade fail.
func marketplaceFake(env *harness.InstallEnv, r *installRecorder, upgradeErr error) {
	upgraded := false
	env.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		c := name + " " + strings.Join(args, " ")
		switch {
		case c == "codex plugin marketplace upgrade crosstalk" && upgradeErr != nil:
			r.calls = append(r.calls, c)
			return nil, upgradeErr
		case c == "codex plugin marketplace upgrade crosstalk":
			upgraded = true
		case c == "codex plugin marketplace add "+env.Marketplace && !upgraded:
			r.calls = append(r.calls, c)
			return nil, errors.New(c + ": exit status 1: " + differentSource)
		}
		return r.run(ctx, name, args...)
	}
}

func checkoutDir(t *testing.T, env harness.InstallEnv, files ...string) {
	t.Helper()
	d := filepath.Join(codexHome(env.Home), ".tmp", "marketplaces", "crosstalk")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		os.WriteFile(filepath.Join(d, f), nil, 0o600)
	}
}

func TestCodexPluginRecoversEmptiedCheckout(t *testing.T) {
	env, r := codexEnv(t, "config-none.toml")
	checkoutDir(t, env)
	marketplaceFake(&env, r, nil)
	if err := pluginStep(env).Apply(context.Background()); err != nil {
		t.Fatalf("an emptied marketplace checkout was not recovered: %v\ncalls: %v", err, r.calls)
	}
	want := []string{
		"codex plugin marketplace add GoodOlClint/CrossTalk",
		"codex plugin marketplace upgrade crosstalk",
		"codex plugin marketplace add GoodOlClint/CrossTalk",
		"codex plugin add crosstalk@crosstalk",
	}
	if !slices.Equal(r.calls, want) {
		t.Fatalf("calls = %v, want %v", r.calls, want)
	}
}

func TestCodexPluginRecoveryKeepsBothErrors(t *testing.T) {
	env, r := codexEnv(t, "config-none.toml")
	marketplaceFake(&env, r, errors.New("could not resolve host github.com"))
	err := pluginStep(env).Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "different source") || !strings.Contains(err.Error(), "resolve host") {
		t.Fatalf("err = %v, want the add and the upgrade errors", err)
	}
}

func TestCodexPluginDifferentSourceStillFails(t *testing.T) {
	for _, mkt := range []string{"GoodOlClint/CrossTalk", "/scratch/mkt"} {
		env, r := codexEnv(t, "config-none.toml")
		env.Marketplace = mkt
		if !filepath.IsAbs(mkt) {
			checkoutDir(t, env, "AGENTS.md")
		}
		marketplaceFake(&env, r, nil)
		err := pluginStep(env).Apply(context.Background())
		if err == nil || !strings.Contains(err.Error(), "different source") {
			t.Errorf("%s: a marketplace added from another source: err = %v", mkt, err)
		}
		if !errors.Is(err, harness.ErrMarketplaceMismatch) || !strings.Contains(err.Error(), "codex plugin marketplace remove crosstalk") || !strings.Contains(err.Error(), "CROSSTALK_MARKETPLACE="+mkt+" crosstalk install") {
			t.Errorf("%s: err = %v, want ErrMarketplaceMismatch naming the remove and re-add commands", mkt, err)
		}
		if want := []string{"codex plugin marketplace add " + mkt}; !slices.Equal(r.calls, want) {
			t.Errorf("%s: calls = %v, want only %v", mkt, r.calls, want)
		}
	}
}

func TestCodexPluginLocalToGitSwitchIsMismatch(t *testing.T) {
	env, r := codexEnv(t, "config-none.toml")
	marketplaceFake(&env, r, errors.New("marketplace `crosstalk` is not configured as a Git marketplace"))
	err := pluginStep(env).Apply(context.Background())
	if !errors.Is(err, harness.ErrMarketplaceMismatch) {
		t.Fatalf("err = %v, want ErrMarketplaceMismatch when the recovery upgrade refuses a local marketplace", err)
	}
}
