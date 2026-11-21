package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func compatDetect(t *testing.T, npmVersion string, args ...string) (string, error) {
	t.Helper()
	root := releaseRepoRoot(t)
	bin := t.TempDir()
	fake := "#!/bin/sh\necho " + npmVersion + "\n"
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, "scripts", "harness-compat.sh"), append([]string{"detect", filepath.Join(root, ".github", "harness-compat.json")}, args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func TestHarnessCompatDetect(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	cases := []struct {
		name   string
		npm    string
		args   []string
		want   string
		wantOK bool
	}{
		{"unverified record is changed", "1.2.3", []string{"codex", "", "", ""}, `[{"harness":"codex","version":"1.2.3"}]`, true},
		{"equal to recorded ends", "1.2.3", []string{"codex", "", "", "1.2.3"}, `[]`, true},
		{"force reruns an equal version", "1.2.3", []string{"codex", "", "true", "1.2.3"}, `[{"harness":"codex","version":"1.2.3"}]`, true},
		{"forced version skips npm", "9.9.9", []string{"codex", "0.171.0", "", ""}, `[{"harness":"codex","version":"0.171.0"}]`, true},
		{"all covers both", "1.2.3", []string{"all", "", "", "1.2.3"}, `[]`, true},
		{"forced version needs one harness", "1.2.3", []string{"all", "0.171.0", "", ""}, "", false},
		{"forced version must be semver", "1.2.3", []string{"codex", "1.2.3; rm -rf /", "", ""}, "", false},
		{"npm garbage refused", "latest", []string{"codex", "", "", ""}, "", false},
		{"unknown harness refused", "1.2.3", []string{"cursor", "", "", ""}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := compatDetect(t, c.npm, c.args...)
			if (err == nil) != c.wantOK {
				t.Fatalf("err = %v, want ok=%v\n%s", err, c.wantOK, out)
			}
			if c.wantOK && out != c.want {
				t.Fatalf("got %s, want %s", out, c.want)
			}
		})
	}
}

func TestHarnessCompatWorkflowSafe(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), ".github", "workflows", "harness-compat.yml"))
	if err != nil {
		t.Fatal(err)
	}
	w := string(b)
	for _, bad := range []string{"pull_request", "cache: true", "actions/cache"} {
		if strings.Contains(w, bad) {
			t.Errorf("workflow must not contain %q", bad)
		}
	}
	for _, want := range []string{"workflow_dispatch:", "permissions:\n  contents: read", "persist-credentials: false", "cache: false", "concurrency:"} {
		if !strings.Contains(w, want) {
			t.Errorf("workflow lacks %q", want)
		}
	}
	if strings.Contains(w, "schedule:") {
		t.Error("the workflow is dispatch-only until slice 5")
	}
	i := strings.Index(w, "\n  live-linux:")
	if i < 0 {
		t.Fatal("no live-linux job")
	}
	if pre := w[:i]; strings.Contains(pre, "secrets.") || strings.Contains(pre, "environment:") {
		t.Error("only live-linux may read secrets or enter an environment")
	}
	live := w[i:]
	for _, want := range []string{"environment: harness-compat\n", "secrets.OPENROUTER_API_KEY", "ran=false", "must both be set when OPENROUTER_API_KEY is", "steps.gate.outputs.ran == 'true'"} {
		if !strings.Contains(live, want) {
			t.Errorf("live-linux lacks %q", want)
		}
	}
	if strings.Contains(live, "echo \"$OPENROUTER_API_KEY\"") || strings.Contains(live, "set -x") {
		t.Error("live-linux must not echo the key")
	}
}

func journalLine(t *testing.T, v map[string]any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func TestHarnessCompatAssertRoundtrip(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	ask := func(from, to, id string) map[string]any {
		return map[string]any{"type": "outbound", "kind": "ask", "to": to, "message_id": id}
	}
	receipt := func(id string) map[string]any { return map[string]any{"type": "receipt", "in_reply_to": id} }
	cases := []struct {
		name   string
		claude []map[string]any
		codex  []map[string]any
		ok     bool
	}{
		{"both directions replied", []map[string]any{ask("c", "cod", "m1"), receipt("m1")}, []map[string]any{ask("cod", "cla", "m2"), receipt("m2")}, true},
		{"accepted ask without a reply", []map[string]any{ask("c", "cod", "m1"), receipt("m1")}, []map[string]any{ask("cod", "cla", "m2")}, false},
		{"reply to some other id", []map[string]any{ask("c", "cod", "m1"), receipt("m9")}, []map[string]any{ask("cod", "cla", "m2"), receipt("m2")}, false},
		{"inform is not an ask", []map[string]any{{"type": "outbound", "kind": "inform", "to": "cod", "message_id": "m1"}, receipt("m1")}, []map[string]any{ask("cod", "cla", "m2"), receipt("m2")}, false},
		{"ask to the wrong agent", []map[string]any{ask("c", "other", "m1"), receipt("m1")}, []map[string]any{ask("cod", "cla", "m2"), receipt("m2")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := t.TempDir()
			for _, d := range []string{"agents", "journal"} {
				if err := os.MkdirAll(filepath.Join(state, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, a := range []struct {
				id, harness string
				recs        []map[string]any
			}{{"cla", "claude", c.claude}, {"cod", "codex", c.codex}} {
				if err := os.WriteFile(filepath.Join(state, "agents", a.id+".json"), []byte(journalLine(t, map[string]any{"agent_id": a.id, "harness": a.harness})), 0o644); err != nil {
					t.Fatal(err)
				}
				var j strings.Builder
				for _, r := range a.recs {
					j.WriteString(journalLine(t, r))
				}
				if err := os.WriteFile(filepath.Join(state, "journal", a.id+".jsonl"), []byte(j.String()), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			root := releaseRepoRoot(t)
			out, err := exec.Command(filepath.Join(root, "scripts", "harness-compat-live.sh"), "assert-roundtrip", state).CombinedOutput()
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v\n%s", err, c.ok, out)
			}
		})
	}
}

func TestHarnessCompatTier0CodexFailsClosed(t *testing.T) {
	cases := []struct {
		name, version, exec string
		ok                  bool
	}{
		{"banner before version", "echo banner; echo 'codex-cli 0.171.0'", `touch "$CODEX_HOME/queue_1.sqlite"`, false},
		{"unparseable", "echo 'codex 0.171.0'", "true", false},
		{"no queue created", "echo 'codex-cli 0.171.0'", "true", false},
		{"queue created", "echo 'codex-cli 0.171.0'", `touch "$CODEX_HOME/queue_1.sqlite"`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin, home := t.TempDir(), t.TempDir()
			fake := "#!/bin/sh\ncase \"$1\" in --version) " + c.version + " ;; exec) " + c.exec + " ;; esac\n"
			if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			root := releaseRepoRoot(t)
			cmd := exec.Command(filepath.Join(root, "scripts", "harness-compat.sh"), "tier0-codex", home)
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v\n%s", err, c.ok, out)
			}
		})
	}
}

func TestHarnessCompatWriteCodexConfigKeepsRootKeys(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	home := t.TempDir()
	root := releaseRepoRoot(t)
	if out, err := exec.Command(filepath.Join(root, "scripts", "harness-compat-live.sh"), "write-codex-config", home, "vendor/model").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cfg := filepath.Join(home, ".codex", "config.toml")
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n[mcp_servers.crosstalk]\ncommand = \"/x/crosstalk\"\nargs = [\"mcp\"]\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	py := "import sys,tomllib\nc=tomllib.load(open(sys.argv[1],'rb'))\nassert c['model']=='vendor/model' and c['model_provider']=='openrouter' and 'crosstalk' in c['mcp_servers'] and c['model_providers']['openrouter']['wire_api']=='responses'\n"
	if out, err := exec.Command("python3", "-I", "-c", py, cfg).CombinedOutput(); err != nil {
		t.Fatalf("root keys lost: %v\n%s", err, out)
	}
}

func TestHarnessCompatStageLogsScrubsNestedKey(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	raw, dest := t.TempDir(), filepath.Join(t.TempDir(), "out")
	const key = "sk-or-v1-dummykey"
	if err := os.MkdirAll(filepath.Join(raw, "state", "journal"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"state/journal/a.jsonl", "top.log"} {
		if err := os.WriteFile(filepath.Join(raw, f), []byte(`{"text":"leak `+key+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"), "stage-logs", raw, dest)
	cmd.Env = append(os.Environ(), "OPENROUTER_API_KEY="+key)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, f := range []string{"state/journal/a.jsonl", "top.log"} {
		b, err := os.ReadFile(filepath.Join(dest, f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), key) || !strings.Contains(string(b), "***") {
			t.Errorf("%s not scrubbed: %s", f, b)
		}
	}
}

func TestHarnessCompatPair(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\necho 5.6.7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), "pair", `[{"harness":"codex","version":"0.171.0"}]`)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), `{"codex":"0.171.0","claude-code":"5.6.7"}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestHarnessCompatScriptsShellcheck(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		if os.Getenv("CI") != "" && runtime.GOOS == "linux" {
			t.Fatal("shellcheck is required on the Linux CI runner")
		}
		t.Skip("shellcheck not on PATH")
	}
	root := releaseRepoRoot(t)
	out, err := exec.Command(sc, "-s", "sh", "-S", "warning", filepath.Join(root, "scripts", "harness-compat.sh"), filepath.Join(root, "scripts", "harness-compat-live.sh")).CombinedOutput()
	if err != nil {
		t.Fatalf("shellcheck:\n%s", out)
	}
}
