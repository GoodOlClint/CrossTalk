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
		key("2:0") + "\ntrusted_hash = \"sha256:stale-index\"\n\n" +
		"[projects.\"/work/app[1]\"]\ntrust_level = \"trusted\"\n"
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
	for _, want := range []string{"sha256:user", "sha256:plugin", "sha256:stale-index", key("0:0"), "trust_level = \"trusted\""} {
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

func TestUninstallRetryDropsTrustRecordAfterFailedConfigWrite(t *testing.T) {
	env, r := codexEnv(t, "")
	hp := hooksPath(env.Home)
	os.MkdirAll(filepath.Dir(hp), 0o700)
	if err := addHook(hp, env.Bin); err != nil {
		t.Fatal(err)
	}
	rec := `[hooks.state."` + hp + `:session_start:0:0"]` + "\ntrusted_hash = \"sha256:ours\"\n"
	os.Mkdir(r.path, 0o700)
	if err := queueWakeRemoveStep(env).Apply(t.Context()); err == nil {
		t.Fatal("expected the config read to fail")
	}
	os.Remove(r.path)
	os.WriteFile(r.path, []byte(rec), 0o600)
	if !hasHook(hp) {
		t.Fatalf("hook removed although the trust record write failed")
	}
	if err := queueWakeRemoveStep(env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(r.path); strings.Contains(string(b), "sha256:ours") {
		t.Errorf("trust record kept after retry:\n%s", b)
	}
}

func TestCodexRegisteredThroughSymlink(t *testing.T) {
	env, r := codexEnv(t, "")
	real := filepath.Join(env.Home, "real", "crosstalk")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.WriteFile(real, nil, 0o755)
	link := filepath.Join(env.Home, "link")
	os.Symlink(real, link)
	os.MkdirAll(filepath.Dir(r.path), 0o700)
	os.WriteFile(r.path, []byte("[mcp_servers.crosstalk]\ncommand = \""+link+"\"\nargs = [\"mcp\"]\n"), 0o600)
	env.Bin = real
	if ok, err := (&Harness{}).Registered(env); err != nil || !ok {
		t.Errorf("Registered = %v, %v; want true for a symlink to the binary", ok, err)
	}
}
