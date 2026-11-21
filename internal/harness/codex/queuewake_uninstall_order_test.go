package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

type uninstallFixture struct {
	env           harness.InstallEnv
	hp, cp, sp    string
	hooks, config string
}

func newUninstallFixture(t *testing.T, config string) uninstallFixture {
	t.Helper()
	env, r := codexEnv(t, "")
	f := uninstallFixture{env: env, hp: hooksPath(env.Home), cp: r.path, sp: filepath.Join(stateDirOf(env), SettingFile)}
	os.MkdirAll(filepath.Dir(f.hp), 0o700)
	if err := addHook(f.hp, env.Bin); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(f.sp), 0o700)
	if err := os.WriteFile(f.sp, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.config = strings.ReplaceAll(config, "HOOKS", f.hp)
	if err := os.WriteFile(f.cp, []byte(f.config), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.hp)
	f.hooks = string(b)
	return f
}

func (f uninstallFixture) assertUnchanged(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(f.sp); err != nil {
		t.Errorf("setting file removed by a refused uninstall: %v", err)
	}
	if b, _ := os.ReadFile(f.hp); string(b) != f.hooks {
		t.Errorf("hooks.json changed: %q", b)
	}
	if b, _ := os.ReadFile(f.cp); string(b) != f.config {
		t.Errorf("config.toml changed: %q", b)
	}
}

func (f uninstallFixture) assertRemoved(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(f.sp); !os.IsNotExist(err) {
		t.Errorf("setting file kept: %v", err)
	}
	if hasHook(f.hp) {
		t.Errorf("hook kept")
	}
	if b, _ := os.ReadFile(f.cp); strings.Contains(string(b), "sha256:ours") {
		t.Errorf("trust record kept:\n%s", b)
	}
}

const orderRecord = `[hooks.state."HOOKS:session_start:0:0"]` + "\ntrusted_hash = \"sha256:ours\"\n"

func TestUninstallOrder_UnclosedRecordThenFixed_RefusedUntouchedThenRemoved(t *testing.T) {
	f := newUninstallFixture(t, `[hooks.state."HOOKS:session_start:0:0"]`+"\nargs = [\n\n[projects.work]\ntrust_level = \"trusted\"\n")
	if err := queueWakeRemoveStep(f.env).Apply(t.Context()); err == nil {
		t.Fatal("expected an error for a record that ends inside an array")
	}
	f.assertUnchanged(t)
	fixed := strings.ReplaceAll(orderRecord, "HOOKS", f.hp)
	if err := os.WriteFile(f.cp, []byte(fixed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := queueWakeRemoveStep(f.env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.assertRemoved(t)
}

func TestUninstallOrder_HooksJSONRefused_NothingTouched(t *testing.T) {
	f := newUninstallFixture(t, orderRecord)
	bad := `{"hooks":{"SessionStart":"nope"}}`
	if err := os.WriteFile(f.hp, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	f.hooks = bad
	if err := queueWakeRemoveStep(f.env).Apply(t.Context()); err == nil {
		t.Fatal("expected a refusal for a hooks.json of the wrong shape")
	}
	f.assertUnchanged(t)
}

func TestUninstallOrder_ConfigWriteFails_NothingTouchedThenRemovedOnRetry(t *testing.T) {
	f := newUninstallFixture(t, orderRecord)
	dir := filepath.Dir(f.cp)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			os.Chmod(dir, 0o700)
		}
	}
	defer restore()
	if os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600) == nil {
		t.Skip("directory permissions do not stop writes here")
	}
	if err := queueWakeRemoveStep(f.env).Apply(t.Context()); err == nil {
		t.Fatal("expected the config write to fail")
	}
	f.assertUnchanged(t)
	restore()
	if err := queueWakeRemoveStep(f.env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.assertRemoved(t)
}
