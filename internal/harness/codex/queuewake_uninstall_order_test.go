package codex

import (
	"errors"
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
	if b, err := os.ReadFile(f.cp); err != nil || string(b) != orderUnrelated {
		t.Errorf("config.toml = %q, %v; want only the unrelated table", b, err)
	}
}

const orderUnrelated = "\n[projects.work]\ntrust_level = \"trusted\"\n"

const orderRecord = `[hooks.state."HOOKS:session_start:0:0"]` + "\ntrusted_hash = \"sha256:ours\"\n" + orderUnrelated

func TestUninstallOrder_UnclosedRecordThenFixed_RefusedUntouchedThenRemoved(t *testing.T) {
	f := newUninstallFixture(t, `[hooks.state."HOOKS:session_start:0:0"]`+"\nargs = [\n\n[projects.work]\ntrust_level = \"trusted\"\n")
	if err := queueWakeRemoveStep(f.env).Apply(t.Context()); err == nil {
		t.Fatal("expected an error for a record that ends inside an array")
	}
	f.assertUnchanged(t)
	fixed := strings.ReplaceAll(orderRecord, "HOOKS", f.hp)
	f.config = fixed
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

func TestUninstallOrder_HooksWriteFailsAfterConfigRewrite_BothRestored(t *testing.T) {
	f := newUninstallFixture(t, strings.Replace(orderRecord, "session_start:0:0", "session_start:1:0", 1))
	scratch := filepath.Join(t.TempDir(), "hooks.json")
	other := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo user"}]}]}}`
	if err := os.WriteFile(scratch, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := addHook(scratch, f.env.Bin); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(scratch)
	if err := os.WriteFile(f.hp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	f.hooks = string(b)
	os.Remove(f.hp + ".crosstalk-backup")
	if err := os.Mkdir(f.hp+".crosstalk-backup", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := queueWakeRemoveStep(f.env).Apply(t.Context()); err == nil {
		t.Fatal("expected the hooks.json write to fail")
	}
	f.assertUnchanged(t)
	os.Remove(f.hp + ".crosstalk-backup")
	if err := queueWakeRemoveStep(f.env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.assertRemoved(t)
}

func TestUninstallOrder_SettingFileDeleteFails_BothRestored(t *testing.T) {
	f := newUninstallFixture(t, orderRecord)
	dir := filepath.Dir(f.sp)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if err := os.Remove(filepath.Join(dir, "no-such-file")); err == nil || os.IsNotExist(err) == false {
		t.Skip("directory permissions do not stop deletes here")
	}
	if err := queueWakeRemoveStep(f.env).Apply(t.Context()); err == nil {
		t.Fatal("expected the setting file delete to fail")
	}
	f.assertUnchanged(t)
	os.Chmod(dir, 0o700)
	if err := queueWakeRemoveStep(f.env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.assertRemoved(t)
}

func TestUninstallOrder_RollbackAlsoFails_BothErrorsSurface(t *testing.T) {
	f := newUninstallFixture(t, strings.Replace(orderRecord, "session_start:0:0", "session_start:1:0", 1))
	scratch := filepath.Join(t.TempDir(), "hooks.json")
	other := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo user"}]}]}}`
	os.WriteFile(scratch, []byte(other), 0o600)
	if err := addHook(scratch, f.env.Bin); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(scratch)
	os.WriteFile(f.hp, b, 0o600)
	if err := os.Mkdir(f.hp+".crosstalk-backup", 0o700); err != nil {
		t.Fatal(err)
	}
	orig := restoreFile
	restoreFile = func(string, []byte) error { return errors.New("injected rollback failure") }
	defer func() { restoreFile = orig }()
	err := queueWakeRemoveStep(f.env).Apply(t.Context())
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"crosstalk-backup", "rollback of " + f.cp + " failed", "injected rollback failure"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}
