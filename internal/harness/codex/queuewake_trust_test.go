package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUninstallDropsOnlyOurHookTrustRecord(t *testing.T) {
	env, r := codexEnv(t, "")
	hp := hooksPath(env.Home)
	os.MkdirAll(filepath.Dir(hp), 0o700)
	other := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"echo user"}]}]}}`
	if err := os.WriteFile(hp, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := addHook(hp, env.Bin); err != nil {
		t.Fatal(err)
	}
	key := func(ref string) string { return `[hooks.state."` + hp + `:session_start:` + ref + `"]` }
	cfg := "[hooks.state]\n\n" + key("0:0") + "\ntrusted_hash = \"sha256:user\"\n\n" +
		key("1:0") + "\ntrusted_hash = \"sha256:ours\"\n\n" +
		`[hooks.state."other@x:hooks.json:session_start:1:0"]` + "\ntrusted_hash = \"sha256:plugin\"\n\n" +
		key("2:0") + "\ntrusted_hash = \"sha256:stale-index\"\n"
	if err := os.WriteFile(r.path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := queueWakeRemoveStep(env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(r.path)
	got := string(b)
	if strings.Contains(got, "sha256:ours") {
		t.Errorf("our hook's trust record kept:\n%s", got)
	}
	for _, want := range []string{"sha256:user", "sha256:plugin", "sha256:stale-index", key("0:0")} {
		if !strings.Contains(got, want) {
			t.Errorf("unrelated record %q removed:\n%s", want, got)
		}
	}
}

func TestUninstallWithoutOurHookLeavesConfigUntouched(t *testing.T) {
	env, r := codexEnv(t, "")
	hp := hooksPath(env.Home)
	os.MkdirAll(filepath.Dir(hp), 0o700)
	cfg := "[hooks.state.\"" + hp + ":session_start:0:0\"]\ntrusted_hash = \"sha256:user\"\n"
	os.WriteFile(r.path, []byte(cfg), 0o600)
	if err := queueWakeRemoveStep(env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(r.path); string(b) != cfg {
		t.Errorf("config changed:\n%s", b)
	}
}
