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
		key("1:0") + "\ntrusted_hash = \"sha256:ours\"\n" +
		"[projects.\"/work/app[1]\"]\ntrust_level = \"trusted\"\n\n" +
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

func TestTrustRecordRemovalIsByteExactAroundIt(t *testing.T) {
	env, r := codexEnv(t, "")
	hp := hooksPath(env.Home)
	os.MkdirAll(filepath.Dir(hp), 0o700)
	if err := addHook(hp, env.Bin); err != nil {
		t.Fatal(err)
	}
	ours := `[hooks.state."` + hp + `:session_start:0:0"]`
	before := "[a]\nx = 1\n\n"
	after := "# keep me\n\n[b]\ny = 2\n"
	if err := os.WriteFile(r.path, []byte(before+ours+"\ntrusted_hash = \"sha256:ours\"\n"+after), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := queueWakeRemoveStep(env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(r.path); string(b) != before+after {
		t.Errorf("config = %q, want %q", b, before+after)
	}
}

func TestTrustRecordHeaderInsideMultilineStringIsKept(t *testing.T) {
	env, r := codexEnv(t, "")
	hp := hooksPath(env.Home)
	os.MkdirAll(filepath.Dir(hp), 0o700)
	if err := addHook(hp, env.Bin); err != nil {
		t.Fatal(err)
	}
	ours := `[hooks.state."` + hp + `:session_start:0:0"]`
	cfg := "[notes]\ntext = \"\"\"\n" + ours + "\nk = 1\n\"\"\"\n\n" + ours + "\ntrusted_hash = \"sha256:ours\"\n"
	if err := os.WriteFile(r.path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := queueWakeRemoveStep(env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := "[notes]\ntext = \"\"\"\n" + ours + "\nk = 1\n\"\"\"\n\n"
	if b, _ := os.ReadFile(r.path); string(b) != want {
		t.Errorf("config = %q, want %q", b, want)
	}
}

func TestTrustRecordScannerHandlesEscapesAndComments(t *testing.T) {
	env, r := codexEnv(t, "")
	hp := hooksPath(env.Home)
	os.MkdirAll(filepath.Dir(hp), 0o700)
	if err := addHook(hp, env.Bin); err != nil {
		t.Fatal(err)
	}
	ours := `[hooks.state."` + hp + `:session_start:0:0"]`
	keep := "# a \"\"\" in a comment\n[n]\ns = \"\"\"\nx \\\"\"\" y\n" + ours + "\n\"\"\"\nl = '''\n" + ours + "\n'''\nq = \"it's\"\n\n"
	if err := os.WriteFile(r.path, []byte(keep+ours+"\ntrusted_hash = \"sha256:ours\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := queueWakeRemoveStep(env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(r.path); string(b) != keep {
		t.Errorf("config = %q, want %q", b, keep)
	}
}

func TestTrustRecordRemovalTakesWholeRecord(t *testing.T) {
	cases := []struct{ name, body, eol string }{
		{"blank line inside", "trusted_hash = \"sha256:ours\"\n\nenabled = true\n", "\n"},
		{"multi-line array", "args = [\n  \"a\",\n  [\"b\"],\n]\nenabled = true\n", "\n"},
		{"array closer looks like header", "args = [\n[1, 2],\n]\nenabled = true\n", "\n"},
		{"quoted and dotted keys", "\"quoted key\" = 1\na.b = 2\n\"x.y\".z = 3\n", "\n"},
		{"comment inside", "# note\ntrusted_hash = \"x\"\n# more\nenabled = true\n", "\n"},
		{"non key value line", "garbage line\nenabled = true\n", "\n"},
		{"quoted values with brackets", "v = \"a [ b\"\nw = 'c ] # d'\nenabled = true\n", "\n"},
		{"multi-line string with header", "v = \"\"\"\n[b]\n\"\"\"\nenabled = true\n", "\n"},
		{"crlf", "trusted_hash = \"sha256:ours\"\n\nenabled = true\n", "\r\n"},
	}
	for _, c := range cases {
		for _, pos := range []string{"middle", "start", "end"} {
			t.Run(c.name+"/"+pos, func(t *testing.T) {
				env, r := codexEnv(t, "")
				hp := hooksPath(env.Home)
				os.MkdirAll(filepath.Dir(hp), 0o700)
				if err := addHook(hp, env.Bin); err != nil {
					t.Fatal(err)
				}
				eol := func(s string) string { return strings.ReplaceAll(s, "\n", c.eol) }
				ours := eol(`[hooks.state."` + hp + `:session_start:0:0"]` + "\n" + c.body)
				before, after := eol("[a]\nx = 1\n\n"), eol("# keep me\n\n[b]\ny = 2\n")
				if pos == "start" {
					before = ""
				}
				if pos == "end" {
					after = ""
				}
				if err := os.WriteFile(r.path, []byte(before+ours+after), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := queueWakeRemoveStep(env).Apply(t.Context()); err != nil {
					t.Fatal(err)
				}
				if b, _ := os.ReadFile(r.path); string(b) != before+after {
					t.Errorf("config = %q, want %q", b, before+after)
				}
			})
		}
	}
}

func TestTrustRecordRemovalNoTrailingNewlineAtEOF(t *testing.T) {
	env, r := codexEnv(t, "")
	hp := hooksPath(env.Home)
	os.MkdirAll(filepath.Dir(hp), 0o700)
	if err := addHook(hp, env.Bin); err != nil {
		t.Fatal(err)
	}
	keep := "[a]\nx = 1\n\n"
	if err := os.WriteFile(r.path, []byte(keep+`[hooks.state."`+hp+`:session_start:0:0"]`+"\nenabled = true"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := queueWakeRemoveStep(env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(r.path); string(b) != keep {
		t.Errorf("config = %q, want %q", b, keep)
	}
}

func TestTrustRecordKeepsCommentsAndBlanksBeforeNextHeader(t *testing.T) {
	env, r := codexEnv(t, "")
	hp := hooksPath(env.Home)
	os.MkdirAll(filepath.Dir(hp), 0o700)
	if err := addHook(hp, env.Bin); err != nil {
		t.Fatal(err)
	}
	other := `[hooks.state."other:session_start:0:0"]`
	cfg := `[hooks.state."` + hp + `:session_start:0:0"]` + "\nenabled = true\n\n# about other\n\n" + other + "\nenabled = false\n"
	if err := os.WriteFile(r.path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := queueWakeRemoveStep(env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := "\n# about other\n\n" + other + "\nenabled = false\n"
	if b, _ := os.ReadFile(r.path); string(b) != want {
		t.Errorf("config = %q, want %q", b, want)
	}
}

func TestTrustRecordSingleLineEscapedQuotesDoNotOpenMultilineString(t *testing.T) {
	if got, _ := tomlStringState(`x = "\""""`, ""); got != "" {
		t.Errorf("state after escaped quote = %q, want none", got)
	}
	env, r := codexEnv(t, "")
	hp := hooksPath(env.Home)
	os.MkdirAll(filepath.Dir(hp), 0o700)
	if err := addHook(hp, env.Bin); err != nil {
		t.Fatal(err)
	}
	keep := "[a]\nx = \"\\\"\"\"\"\n\n"
	if err := os.WriteFile(r.path, []byte(keep+`[hooks.state."`+hp+`:session_start:0:0"]`+"\nenabled = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := queueWakeRemoveStep(env).Apply(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(r.path); string(b) != keep {
		t.Errorf("config = %q, want %q", b, keep)
	}
}
