package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
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
	jobs := workflowJobs(w)
	secretJobs := map[string]bool{"key-before": true, "key-after": true, "harness": true, "cross": true}
	for name, body := range jobs {
		has := strings.Contains(body, "secrets.") || strings.Contains(body, "environment:")
		if has != secretJobs[name] {
			t.Errorf("job %s: secrets or environment = %v, want %v", name, has, secretJobs[name])
		}
		if strings.Contains(body, "echo \"$OPENROUTER_API_KEY\"") || strings.Contains(body, "set -x") {
			t.Errorf("job %s must not echo the key", name)
		}
	}
	for _, name := range []string{"harness", "cross"} {
		body := jobs[name]
		for _, want := range []string{"environment: harness-compat\n", "secrets.OPENROUTER_API_KEY", "ran=false", "must both be set when OPENROUTER_API_KEY is", "steps.gate.outputs.ran == 'true'"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	if !strings.Contains(jobs["harness"], "fail-fast: false") || !strings.Contains(jobs["cross"], "legs-passed") {
		t.Error("harness must not fail fast and cross must gate on legs-passed")
	}
}

func TestHarnessCompatLegsGateOnTheirOwnTier0(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), ".github", "workflows", "harness-compat.yml"))
	if err != nil {
		t.Fatal(err)
	}
	w := string(b)
	jobs := workflowJobs(w)
	if !strings.Contains(jobs["tier0"], "name: tier0-${{ matrix.harness }}") {
		t.Error("tier0 jobs must be named tier0-<matrix harness>")
	}
	for _, line := range strings.Split(jobs["harness"], "\n") {
		if strings.Contains(line, "TIER0:") && strings.TrimSpace(line) != "TIER0: tier0-${{ matrix.harness }}" {
			t.Errorf("a leg must gate on its own harness's tier0 job, got %q", strings.TrimSpace(line))
		}
	}
	if !strings.Contains(jobs["harness"], "TIER0: tier0-${{ matrix.harness }}") {
		t.Error("harness lacks the per-harness tier0 gate")
	}
}

func TestHarnessCompatVerdictIsNotRunnerWritten(t *testing.T) {
	root := releaseRepoRoot(t)
	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "harness-compat.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if rep := workflowJobs(string(wf))["report"]; strings.Contains(rep, "download-artifact") || strings.Contains(rep, "result-") || !strings.Contains(rep, "actions/runs/$GITHUB_RUN_ID/jobs") {
		t.Error("report must read the jobs API and no artifact")
	}
	sc, err := os.ReadFile(filepath.Join(root, "scripts", "harness-compat.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sc), "result.json") {
		t.Error("harness-compat.sh must not read a runner-written result file")
	}
}

func TestHarnessCompatAssertOneCodexRow(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	// Codex records an MCP call as an item_completed McpToolCall under payload.item, with the tool result in
	// structuredContent and, as text, in content (observed in a live rollout, Codex 0.171.0).
	call := func(tool, status string, result any) string {
		b, _ := json.Marshal(map[string]any{"payload": map[string]any{"type": "item_completed", "item": map[string]any{"type": "McpToolCall", "server": "crosstalk", "tool": tool, "status": status, "result": result}}})
		return string(b) + "\n"
	}
	listing := func(harnesses ...string) string {
		var rows []map[string]any
		for _, h := range harnesses {
			rows = append(rows, map[string]any{"harness": h, "crosstalk": true})
		}
		text, _ := json.Marshal(map[string]any{"agents": rows})
		return call("list_agents", "completed", map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}, "structuredContent": map[string]any{"agents": rows}})
	}
	textOnly := func(harnesses ...string) string {
		var rows []map[string]any
		for _, h := range harnesses {
			rows = append(rows, map[string]any{"harness": h})
		}
		text, _ := json.Marshal(map[string]any{"agents": rows})
		return call("list_agents", "completed", map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}})
	}
	cases := []struct {
		name  string
		files map[string]string
		want  int
	}{
		{"lone codex row", map[string]string{"recv": listing("codex", "claude")}, 0},
		{"result carried only as content text", map[string]string{"recv": textOnly("codex")}, 0},
		{"ephemeral helper thread shows as a second row", map[string]string{"recv": listing("codex", "codex")}, 1},
		{"another thread's one-row listing does not count", map[string]string{"recv": listing("codex", "codex"), "other": listing("codex")}, 1},
		{"first list_agents output is unreadable", map[string]string{"recv": call("list_agents", "completed", map[string]any{"content": []map[string]any{{"type": "text", "text": "not json"}}})}, 1},
		{"receiver has no rollout yet", map[string]string{"other": listing("codex")}, 2},
		{"another tool's result is not a list_agents result", map[string]string{"recv": call("receive_message", "completed", map[string]any{"structuredContent": map[string]any{"agents": []map[string]any{{"harness": "codex"}}}})}, 2},
		{"first list_agents call failed", map[string]string{"recv": call("list_agents", "failed", nil)}, 1},
		{"a failed first call is not masked by a good retry", map[string]string{"recv": call("list_agents", "failed", nil) + listing("codex")}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "2026", "11", "21")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for ref, body := range c.files {
				if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-21T01-00-00-"+ref+".jsonl"), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			root := filepath.Dir(filepath.Dir(filepath.Dir(dir)))
			err := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"), "assert-one-codex-row", root, "recv").Run()
			got := 0
			if ee, ok := err.(*exec.ExitError); ok {
				got = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("exit %d, want %d", got, c.want)
			}
		})
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
	if err := os.MkdirAll(filepath.Join(raw, "state", "journal"), 0o755); err != nil || os.MkdirAll(filepath.Join(raw, "codex-sessions", "2026", "11", "21"), 0o755) != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"state/journal/a.jsonl", "codex-sessions/2026/11/21/rollout.jsonl", "top.log"} {
		if err := os.WriteFile(filepath.Join(raw, f), []byte(`{"text":"leak `+key+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"), "stage-logs", raw, dest)
	cmd.Env = append(os.Environ(), "OPENROUTER_API_KEY="+key)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, f := range []string{"state/journal/a.jsonl", "codex-sessions/2026/11/21/rollout.jsonl", "top.log"} {
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

func TestHarnessCompatTier0ClaudeSocketPath(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	cases := []struct {
		name, dir string
		makeSock  bool
		ok        bool
		link      bool
	}{
		{"tmp cc-socks (macOS)", "cc-socks", true, true, false},
		{"runtime dir cc-socks (Linux)", "run/user/1001/cc-socks", true, true, false},
		{"outside a cc-socks dir", "elsewhere", true, false, false},
		{"socket file missing", "cc-socks", false, false, false},
		{"symlink to a socket", "cc-socks", true, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin, home := t.TempDir(), t.TempDir()
			// unix socket paths are length-limited, so the socket lives under /tmp.
			sd, err := os.MkdirTemp("/tmp", "ccs")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(sd) })
			dir := filepath.Join(sd, c.dir)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			mk := ""
			if c.makeSock {
				target := `"$SOCK"`
				if c.link {
					target = `"$SOCK.real"`
				}
				mk = `python3 -c 'import socket,sys; s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1]); import time; time.sleep(30)' ` + target + " &"
				if c.link {
					mk += "\nsleep 1\nln -s \"$SOCK.real\" \"$SOCK\""
				}
			}
			fake := "#!/bin/sh\nmkdir -p \"$HOME/.claude/sessions\"\nSOCK=\"" + dir + "/$$.sock\"\n" + mk + "\n" +
				"printf '{\"pid\":%s,\"sessionId\":\"s\",\"kind\":\"interactive\",\"messagingSocketPath\":\"%s\"}' $$ \"$SOCK\" > \"$HOME/.claude/sessions/$$.json\"\nsleep 30\n"
			if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), "tier0-claude", home)
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "COMPAT_WAIT_TRIES=6")
			out, err := cmd.CombinedOutput()
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v\n%s", err, c.ok, out)
			}
		})
	}
}

func TestHarnessCompatKeyUsage(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	bin, out := t.TempDir(), t.TempDir()
	args := filepath.Join(out, "curl-args")
	fake := "#!/bin/sh\necho \"$@\" > " + args + "\nfor a; do case \"$a\" in @*) cat \"${a#@}\" > " + filepath.Join(out, "hdr-seen") + " ;; esac; done\necho '{\"data\":{\"usage\":1.5,\"limit\":10,\"limit_remaining\":8.5,\"label\":\"x\"}}'\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	const key = "sk-or-v1-dummykey"
	cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"), "key-usage", "before", out)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "OPENROUTER_API_KEY="+key)
	stdout, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if strings.Contains(string(stdout), key) {
		t.Errorf("key printed: %s", stdout)
	}
	a, _ := os.ReadFile(args)
	if strings.Contains(string(a), key) {
		t.Errorf("key on the curl command line: %s", a)
	}
	if h, _ := os.ReadFile(filepath.Join(out, "hdr-seen")); !strings.Contains(string(h), key) {
		t.Errorf("auth header file did not carry the key: %q", h)
	}
	b, err := os.ReadFile(filepath.Join(out, "key-before.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(b)), `{"usage":1.5,"limit":10,"limit_remaining":8.5}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestHarnessCompatKeyUsageLookupFailure(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	for name, body := range map[string]string{"curl fails": "exit 22", "no usage field": "echo '{\"data\":{}}'", "empty body": "true"} {
		t.Run(name, func(t *testing.T) {
			bin, out := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"), "key-usage", "before", out)
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "OPENROUTER_API_KEY=sk-or-v1-x")
			stdout, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, stdout)
			}
			if !strings.Contains(string(stdout), "unavailable") {
				t.Errorf("want unavailable, got %s", stdout)
			}
			if _, err := os.Stat(filepath.Join(out, "key-before.json")); err == nil {
				t.Error("a snapshot was saved for a failed lookup")
			}
		})
	}
}

func TestHarnessCompatAssertDiscovery(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	row := func(id string, ct bool) string {
		agents := []map[string]any{{"agent_id": id, "harness": "codex", "crosstalk": ct}}
		if !ct {
			agents = append(agents, map[string]any{"agent_id": "bound-sibling", "harness": "codex", "crosstalk": true})
		}
		b, _ := json.Marshal(map[string]any{"agents": agents})
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "content": string(b)}}}})
		return string(line) + "\n"
	}
	cases := []struct {
		name, log string
		ok        bool
	}{
		{"listed and bound", row("abc", true), true},
		{"peer missing from the listing", row("other", true), false},
		{"listed but unbound beside a bound sibling", row("abc", false), false},
		{"no listing at all", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "claude.jsonl")
			if err := os.WriteFile(f, []byte(c.log), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"), "assert-discovery", f, "abc").CombinedOutput()
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v\n%s", err, c.ok, out)
			}
		})
	}
}

func TestHarnessCompatLegsAndReport(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	script := filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh")
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(script, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	legs := run("legs", `[{"harness":"codex","version":"0.171.0"},{"harness":"claude-code","version":"2.1.330"}]`)
	if want := `[{"leg":"codex-linux","harness":"codex","version":"0.171.0","runner":"ubuntu-latest"},{"leg":"codex-macos","harness":"codex","version":"0.171.0","runner":"macos-latest"},{"leg":"claude","harness":"claude-code","version":"2.1.330","runner":"ubuntu-latest"}]`; legs != want {
		t.Fatalf("legs = %s, want %s", legs, want)
	}
	rehearsal := run("legs", `[{"harness":"codex","version":"0.171.0"}]`, "0.167.0")
	if want := `[{"leg":"codex-linux","harness":"codex","version":"0.171.0","runner":"ubuntu-latest"},{"leg":"codex-macos","harness":"codex","version":"0.167.0","runner":"macos-latest"}]`; rehearsal != want {
		t.Fatalf("legs with a pinned queue version = %s, want %s", rehearsal, want)
	}
	if onlyClaude := run("legs", `[{"harness":"claude-code","version":"2.1.330"}]`, "0.167.0"); strings.Contains(onlyClaude, "codex-macos") {
		t.Fatalf("a Claude-only change must not run the Codex macOS leg: %s", onlyClaude)
	}
	// The report cases below use two legs so each fixture stays small.
	legs = `[{"leg":"codex-linux","harness":"codex","version":"0.171.0"},{"leg":"claude","harness":"claude-code","version":"2.1.330"}]`
	job := func(name, conclusion string, steps map[string]string) map[string]any {
		var st []map[string]any
		for n, c := range steps {
			st = append(st, map[string]any{"name": n, "conclusion": c})
		}
		return map[string]any{"name": name, "conclusion": conclusion, "steps": st}
	}
	write := func(jobs ...map[string]any) string {
		t.Helper()
		var b strings.Builder
		for _, j := range jobs {
			b.WriteString(journalLine(t, j))
		}
		f := filepath.Join(t.TempDir(), "jobs.json")
		if err := os.WriteFile(f, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return f
	}
	ran := map[string]string{"Run the leg": "success"}
	skipped := map[string]string{"Run the leg": "skipped"}
	crossRan := map[string]string{"Ask each way between Claude and Codex": "success"}
	crossSkipped := map[string]string{"Ask each way between Claude and Codex": "skipped"}
	cases := []struct {
		name string
		jobs []map[string]any
		want string
	}{
		{"claude passes, codex fails, cross skipped",
			[]map[string]any{job("harness-claude", "success", ran), job("harness-codex-linux", "failure", map[string]string{"Run the leg": "failure"}), job("cross", "success", crossSkipped)},
			`{"claude-code":{"version":"2.1.330","legs":{"claude":"pass"},"leg_versions":{"claude":"2.1.330"},"pass":true},"codex":{"version":"0.171.0","legs":{"codex-linux":"fail"},"leg_versions":{"codex-linux":"0.171.0"},"pass":false},"cross":"skipped"}`},
		{"missing key skips every step and is not a pass",
			[]map[string]any{job("harness-claude", "success", skipped), job("harness-codex-linux", "success", skipped), job("cross", "success", crossSkipped)},
			`{"claude-code":{"version":"2.1.330","legs":{"claude":"skipped"},"leg_versions":{"claude":"2.1.330"},"pass":false},"codex":{"version":"0.171.0","legs":{"codex-linux":"skipped"},"leg_versions":{"codex-linux":"0.171.0"},"pass":false},"cross":"skipped"}`},
		{"success job whose test step did not succeed is not a pass",
			[]map[string]any{job("harness-claude", "success", map[string]string{"Run the leg": "failure"}), job("harness-codex-linux", "success", ran), job("cross", "success", crossRan)},
			`{"claude-code":{"version":"2.1.330","legs":{"claude":"fail"},"leg_versions":{"claude":"2.1.330"},"pass":false},"codex":{"version":"0.171.0","legs":{"codex-linux":"pass"},"leg_versions":{"codex-linux":"0.171.0"},"pass":true},"cross":"pass"}`},
		{"a leg with no job reads as fail",
			[]map[string]any{job("harness-codex-linux", "success", ran)},
			`{"claude-code":{"version":"2.1.330","legs":{"claude":"fail"},"leg_versions":{"claude":"2.1.330"},"pass":false},"codex":{"version":"0.171.0","legs":{"codex-linux":"pass"},"leg_versions":{"codex-linux":"0.171.0"},"pass":true},"cross":"fail"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := run("report", write(c.jobs...), legs); got != c.want {
				t.Fatalf("report = %s\nwant     %s", got, c.want)
			}
		})
	}
	// A queue leg that ran another Codex version does not make the candidate pass.
	pinned := `[{"leg":"codex-linux","harness":"codex","version":"0.171.0"},{"leg":"codex-macos","harness":"codex","version":"0.167.0"}]`
	allRan := write(job("harness-codex-linux", "success", ran), job("harness-codex-macos", "success", ran), job("cross", "success", crossRan))
	if got := run("report", allRan, pinned); !strings.Contains(got, `"pass":false`) || !strings.Contains(got, `"codex-macos":"0.167.0"`) {
		t.Fatalf("a queue leg on another version must not pass the candidate: %s", got)
	}
	same := `[{"leg":"codex-linux","harness":"codex","version":"0.171.0"},{"leg":"codex-macos","harness":"codex","version":"0.171.0"}]`
	if got := run("report", allRan, same); !strings.Contains(got, `"pass":true`) {
		t.Fatalf("both Codex legs on the candidate version pass it: %s", got)
	}
	both := write(job("harness-claude", "success", ran), job("harness-codex-linux", "success", ran))
	if out, err := exec.Command(script, "legs-passed", both, legs).CombinedOutput(); err != nil {
		t.Fatalf("legs-passed with both legs passing: %v\n%s", err, out)
	}
	one := write(job("harness-claude", "success", ran), job("harness-codex-linux", "success", skipped))
	if err := exec.Command(script, "legs-passed", one, legs).Run(); err == nil {
		t.Fatal("legs-passed must fail when a leg did not run")
	}
}

func TestHarnessCompatAgentID(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(id, harness string) {
		line := journalLine(t, map[string]any{"agent_id": id, "harness": harness, "started_at": "2026-11-21T01:00:00Z"})
		if err := os.WriteFile(filepath.Join(state, "agents", id+".json"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh")
	id := func(args ...string) (string, error) {
		out, err := exec.Command(script, append([]string{"agent-id", state}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	write("zzz-receiver", "codex")
	if got, err := id("codex"); err != nil || got != "zzz-receiver" {
		t.Fatalf("lone codex = %q, %v", got, err)
	}
	write("aaa-asker", "codex")
	if _, err := id("codex"); err == nil {
		t.Fatal("two codex descriptors must fail without an exclusion")
	}
	if got, err := id("codex", "zzz-receiver"); err != nil || got != "aaa-asker" {
		t.Fatalf("asker with equal start times and the receiver sorting last = %q, %v", got, err)
	}
	write("mmm-third", "codex")
	if _, err := id("codex", "zzz-receiver"); err == nil {
		t.Fatal("a third descriptor must make the choice ambiguous")
	}
}

// notTmpDir returns a fresh directory whose resolved path is not under /tmp, or skips the test.
func notTmpDir(t *testing.T, prefix string) string {
	t.Helper()
	tmp, _ := filepath.EvalSymlinks("/tmp")
	for _, base := range []string{releaseRepoRoot(t), os.Getenv("HOME")} {
		if base == "" {
			continue
		}
		d, err := os.MkdirTemp(base, prefix)
		if err != nil {
			continue
		}
		real, _ := filepath.EvalSymlinks(d)
		if real == tmp || strings.HasPrefix(real, tmp+string(os.PathSeparator)) {
			os.RemoveAll(d)
			continue
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		return d
	}
	t.Skip("no writable directory outside /tmp")
	return ""
}

func tmpHomeUnder(t *testing.T) func() string {
	return func() string {
		d, err := os.MkdirTemp("/tmp", "ptyrun")
		if err != nil {
			t.Skip("no /tmp")
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		return d
	}
}

func TestHarnessCompatPtyrun(t *testing.T) {
	tmpHome := tmpHomeUnder(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	root := releaseRepoRoot(t)
	dialog, err := os.ReadFile(filepath.Join(root, "scripts", "testdata", "codex-hooks-dialog.txt"))
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(root, "scripts", "ptyrun.py"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`\("([^"]+)", b"`).FindAllStringSubmatch(string(src), -1) {
		if !strings.Contains(string(dialog), m[1]) {
			t.Errorf("ptyrun matches %q, which the captured Codex dialog does not contain", m[1])
		}
	}
	// The fake program is Python, not a shell script: dash's read builtin did not return for a second line typed on a pty.
	prog := filepath.Join(t.TempDir(), "dialog.py")
	script := "import sys\nsys.stdout.write(open(sys.argv[1]).read())\nsys.stdout.flush()\n" +
		"a = sys.stdin.readline().strip()\nprint('ANSWER=' + a, flush=True)\n" +
		"m = sys.stdin.readline().strip()\nprint('MORE=' + m, flush=True)\n"
	if err := os.WriteFile(prog, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	fixtureTxt := filepath.Join(root, "scripts", "testdata", "codex-hooks-dialog.txt")
	run := func(home, cwd string, extraEnv ...string) string {
		dir := t.TempDir()
		fifo := filepath.Join(dir, "in")
		if err := exec.Command("mkfifo", fifo).Run(); err != nil {
			t.Fatal(err)
		}
		logf := filepath.Join(dir, "log")
		cmd := exec.Command(filepath.Join(root, "scripts", "ptyrun.py"), logf, fifo, "python3", prog, fixtureTxt)
		var env []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "CODEX_HOME=") && !strings.HasPrefix(kv, "RUNNER_TEMP=") && !strings.HasPrefix(kv, "HOME=") {
				env = append(env, kv)
			}
		}
		cmd.Env = append(append(env, "HOME="+home), extraEnv...)
		cmd.Dir = cwd
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Poll the log instead of sleeping: a race-enabled CI run is slow, and a fixed sleep killed the program mid-output.
		waitFor := func(substr string, limit time.Duration) bool {
			for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
				if b, _ := os.ReadFile(logf); strings.Contains(string(b), substr) {
					return true
				}
			}
			return false
		}
		waitFor("enter confirm", 15*time.Second)
		// A refusal writes its note as soon as the driver starts, so look for that first; only an answering driver needs the long wait.
		if !waitFor("not a disposable", 1500*time.Millisecond) {
			waitFor("ANSWER=2", 15*time.Second)
		}
		f, _ := os.OpenFile(fifo, os.O_RDWR, 0)
		f.WriteString("typed\r")
		f.Close()
		waitFor("MORE=typed", 4*time.Second)
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
		b, _ := os.ReadFile(logf)
		return string(b)
	}
	good := tmpHome()
	if out := run(good, good, "CODEX_HOME="+filepath.Join(good, ".codex")); !strings.Contains(out, "ANSWER=2") || !strings.Contains(out, "MORE=typed") {
		t.Errorf("in a disposable home the dialog is answered with 2 and fifo input is forwarded:\n%s", out)
	}
	refused := func(name, home, cwd string, extraEnv ...string) {
		t.Helper()
		if out := run(home, cwd, extraEnv...); strings.Contains(out, "ANSWER=2") || !strings.Contains(out, "not a disposable") {
			t.Errorf("%s: nothing may be answered:\n%s", name, out)
		}
	}
	// Each case below is refused by exactly one rule: the cwd and the other variables are set so every other rule would pass.
	h := tmpHome()
	refused("CODEX_HOME outside the scratch home", h, h, "CODEX_HOME=/Users/someone/.codex")
	h = tmpHome()
	if err := os.Mkdir(filepath.Join(h, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	refused("CODEX_HOME unset (cwd is inside the home, so an empty path would resolve inside it)", h, filepath.Join(h, ".codex"))
	scratchCwd := tmpHome()
	refused("HOME empty (cwd is a scratch dir, so an empty path would resolve to it)", "", scratchCwd, "CODEX_HOME="+filepath.Join(scratchCwd, ".codex"))
	refused("a real home outside the temp dir", "/Users/someone", tmpHome(), "CODEX_HOME=/Users/someone/.codex")
	refused("RUNNER_TEMP does not make a real home disposable", "/Users/someone", tmpHome(), "RUNNER_TEMP=/Users", "CODEX_HOME=/Users/someone/.codex")
	h = tmpHome()
	if err := os.Mkdir(filepath.Join(h, "profile"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tmpHome(), filepath.Join(h, ".codex")); err != nil {
		t.Fatal(err)
	}
	refused("$HOME/.codex is a symlink out of the scratch home while CODEX_HOME is inside", h, h, "CODEX_HOME="+filepath.Join(h, "profile"))
	notTmp := notTmpDir(t, "nottmp")
	notTmpHome := filepath.Join(notTmp, "home")
	if err := os.Mkdir(notTmpHome, 0o755); err != nil {
		t.Fatal(err)
	}
	refused("TMPDIR naming the parent of a non-/tmp home does not make it disposable", notTmpHome, notTmpHome, "TMPDIR="+notTmp, "CODEX_HOME="+filepath.Join(notTmpHome, ".codex"))
	link := filepath.Join(tmpHome(), "homelink")
	if err := os.Symlink("/Users", link); err != nil {
		t.Fatal(err)
	}
	refused("HOME is a symlink to a real home", link, tmpHome(), "CODEX_HOME="+filepath.Join(link, ".codex"))
}

func TestHarnessCompatAssertMailboxAndDeliveryMode(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	script := filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh")
	setup := func(recv ...string) string {
		state := t.TempDir()
		if err := os.MkdirAll(filepath.Join(state, "journal"), 0o755); err != nil {
			t.Fatal(err)
		}
		send := journalLine(t, map[string]any{"type": "outbound", "kind": "inform", "to": "rcv", "message_id": "m1"})
		if err := os.WriteFile(filepath.Join(state, "journal", "snd.jsonl"), []byte(send), 0o644); err != nil {
			t.Fatal(err)
		}
		var j strings.Builder
		for _, st := range recv {
			j.WriteString(journalLine(t, map[string]any{"type": "dedupe", "message_id": "m1", "state": st}))
		}
		if err := os.WriteFile(filepath.Join(state, "journal", "rcv.jsonl"), []byte(j.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return state
	}
	for name, c := range map[string]struct {
		states []string
		ok     bool
	}{
		"mailboxed then delivered":           {[]string{"mailboxed", "delivered"}, true},
		"mailboxed then confirmed":           {[]string{"mailboxed", "confirmed"}, true},
		"mailboxed only":                     {[]string{"mailboxed"}, false},
		"delivered by the tool output route": {[]string{"accepted"}, false},
		"nothing recorded":                   {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			err := exec.Command(script, "assert-mailbox", setup(c.states...), "snd", "rcv").Run()
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
		})
	}
	row := func(mode string) string {
		b, _ := json.Marshal(map[string]any{"agents": []map[string]any{{"agent_id": "abc", "delivery_mode": mode}}})
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{{"type": "tool_result", "content": string(b)}}}})
		return string(line) + "\n"
	}
	for name, c := range map[string]struct {
		mode string
		ok   bool
	}{"queue wake": {"queue-wake (experimental)", true}, "no mode": {"", false}, "another mode": {"tool-output", false}} {
		t.Run("delivery mode "+name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "claude.jsonl")
			if err := os.WriteFile(f, []byte(row(c.mode)), 0o644); err != nil {
				t.Fatal(err)
			}
			err := exec.Command(script, "assert-delivery-mode", f, "abc").Run()
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

func TestHarnessCompatLegsCallTheOneRowCheckAndItIsFatal(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for leg, call := range map[string]string{"codex-linux": `one_row_check "$recv"`, "cross": `one_row_check "$codex_id"`} {
		i := strings.Index(src, "\n\t"+leg+")")
		if i < 0 {
			t.Fatalf("no %s leg", leg)
		}
		rest := src[i+1:]
		if j := strings.Index(rest[1:], "\n\t;;"); j >= 0 {
			rest = rest[:j+1]
		}
		if strings.Count(rest, call) != 1 {
			t.Errorf("%s must call %s exactly once", leg, call)
		}
	}
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "one_row_check") && strings.Contains(line, "||") && !strings.Contains(line, "one_row_check()") {
			t.Errorf("one_row_check must stay fatal: %q", strings.TrimSpace(line))
		}
	}
	fn := src[strings.Index(src, "\tone_row_check() {"):]
	fn = fn[:strings.Index(fn, "\n\t}")]
	if !strings.Contains(fn, `return "$rc"`) || !strings.Contains(fn, "return 1") || strings.Contains(fn, "|| true") {
		t.Error("one_row_check must return the checker's failure")
	}
}

func TestHarnessCompatMacosLegHelpers(t *testing.T) {
	root := releaseRepoRoot(t)
	script := filepath.Join(root, "scripts", "harness-compat-live.sh")

	t.Run("ptyrun path is absolute whatever the caller's cwd", func(t *testing.T) {
		cmd := exec.Command("sh", script, "ptyrun-path")
		cmd.Dir = filepath.Join(root, "cmd")
		out, err := cmd.Output()
		got := strings.TrimSpace(string(out))
		if err != nil || !filepath.IsAbs(got) || got != filepath.Join(root, "scripts", "ptyrun.py") {
			t.Fatalf("ptyrun-path = %q, %v", got, err)
		}
		cmd = exec.Command("sh", "../scripts/harness-compat-live.sh", "ptyrun-path")
		cmd.Dir = filepath.Join(root, "cmd")
		out, err = cmd.Output()
		if got := strings.TrimSpace(string(out)); err != nil || got != filepath.Join(root, "scripts", "ptyrun.py") {
			t.Fatalf("with a relative script path, ptyrun-path = %q, %v", got, err)
		}
	})

	t.Run("assert-wake wants a collected generation", func(t *testing.T) {
		if _, err := exec.LookPath("jq"); err != nil {
			t.Skip("jq not on PATH")
		}
		for state, ok := range map[string]bool{"collected": true, "queued": false, "admitted": false, "failed": false, "": false} {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "codex-wakes"), 0o755); err != nil {
				t.Fatal(err)
			}
			if state != "" {
				if err := os.WriteFile(filepath.Join(dir, "codex-wakes", "th1.json"), []byte(`{"thread":"th1","gen":1,"state":"`+state+`"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := exec.Command(script, "assert-wake", dir, "th1").Run()
			if (err == nil) != ok {
				t.Errorf("state %q: err = %v, want ok=%v", state, err, ok)
			}
		}
	})

	t.Run("rollout-ready needs a task_complete event in that session's own rollout, never model text", func(t *testing.T) {
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("python3 not on PATH")
		}
		// Shapes from a live Codex 0.167.0 rollout: payload-level task_started and task_complete events.
		event := func(kind string) string {
			b, _ := json.Marshal(map[string]any{"payload": map[string]any{"type": kind, "turn_id": "t1", "last_agent_message": "READY"}})
			return string(b) + "\n"
		}
		answer := func(text string) string {
			b, _ := json.Marshal(map[string]any{"payload": map[string]any{"type": "item_completed", "item": map[string]any{"type": "AgentMessage", "phase": "final_answer", "content": []map[string]any{{"type": "Text", "text": text}}}}})
			return string(b) + "\n"
		}
		cases := map[string]struct {
			files map[string]string
			ok    bool
		}{
			"the turn completed":                      {map[string]string{"th1": event("task_started") + answer("anything at all") + event("task_complete")}, true},
			"the turn only started":                   {map[string]string{"th1": event("task_started")}, false},
			"a READY answer without a completed turn": {map[string]string{"th1": event("task_started") + answer("READY")}, false},
			"another session's turn completed":        {map[string]string{"other": event("task_complete"), "th1": event("task_started")}, false},
		}
		for name, c := range cases {
			dir := filepath.Join(t.TempDir(), "2026", "11", "21")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for ref, body := range c.files {
				if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-21T01-00-00-"+ref+".jsonl"), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := exec.Command(script, "rollout-ready", filepath.Dir(filepath.Dir(filepath.Dir(dir))), "th1").Run()
			if (err == nil) != c.ok {
				t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
			}
		}
	})

	t.Run("assert-own-delivery-mode reads the tab's own list_agents result in its own rollout", func(t *testing.T) {
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("python3 not on PATH")
		}
		// Shape from a live Codex 0.167.0 rollout: an item_completed McpToolCall whose result carries the agents as text and structured content.
		listing := func(rows ...map[string]any) string {
			text, _ := json.Marshal(map[string]any{"agents": rows})
			b, _ := json.Marshal(map[string]any{"payload": map[string]any{"type": "item_completed", "item": map[string]any{"type": "McpToolCall", "server": "crosstalk", "tool": "list_agents", "status": "completed",
				"result": map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}, "structuredContent": map[string]any{"agents": rows}}}}})
			return string(b) + "\n"
		}
		self := func(mode string) map[string]any {
			r := map[string]any{"agent_id": "me", "harness": "codex", "crosstalk": true, "reachable": false, "status": ""}
			if mode != "" {
				r["delivery_mode"] = mode
			}
			return r
		}
		cases := map[string]struct {
			files map[string]string
			ok    bool
		}{
			"the tab's own row shows queue-wake (reachable false, status empty)": {map[string]string{"th1": listing(self("queue-wake (experimental)"))}, true},
			"the row has no delivery_mode":                                       {map[string]string{"th1": listing(self(""))}, false},
			"another mode":                                                       {map[string]string{"th1": listing(self("tool-output"))}, false},
			"queue-wake on some other agent's row":                               {map[string]string{"th1": listing(self(""), map[string]any{"agent_id": "other", "delivery_mode": "queue-wake (experimental)"})}, false},
			"another session's rollout shows it":                                 {map[string]string{"th1": "", "other": listing(self("queue-wake (experimental)"))}, false},
		}
		for name, c := range cases {
			dir := filepath.Join(t.TempDir(), "2026", "11", "21")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for ref, body := range c.files {
				if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-21T01-00-00-"+ref+".jsonl"), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := exec.Command(script, "assert-own-delivery-mode", filepath.Dir(filepath.Dir(filepath.Dir(dir))), "th1", "me").Run()
			if (err == nil) != c.ok {
				t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
			}
		}
	})

	t.Run("check-staged needs the wake and claim records of the tested thread", func(t *testing.T) {
		mk := func(wake, claim string) string {
			d := t.TempDir()
			for _, dir := range []string{"codex-wakes", "codex-claims"} {
				if err := os.MkdirAll(filepath.Join(d, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if wake != "" {
				if err := os.WriteFile(filepath.Join(d, "codex-wakes", wake+".json"), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if claim != "" {
				if err := os.WriteFile(filepath.Join(d, "codex-claims", "123-456-"+claim+".json"), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			return d
		}
		for name, c := range map[string]struct {
			dir string
			ok  bool
		}{
			"both records for the thread": {mk("th1", "th1"), true},
			"no wake record":              {mk("", "th1"), false},
			"no claim record":             {mk("th1", ""), false},
			"records of another thread":   {mk("other", "other"), false},
		} {
			err := exec.Command(script, "check-staged", c.dir, "th1").Run()
			if (err == nil) != c.ok {
				t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
			}
		}
	})

	t.Run("a successful macOS leg fails if its staged artifact lacks the records, and asks the tab for a fresh listing when needed", func(t *testing.T) {
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, want := range []string{`check-staged "$logs/state" "$tab_ref"`, `[ "$staged" -eq 0 ] || exit 1`, `Call the crosstalk list_agents tool once more." >&8`} {
			if !strings.Contains(src, want) {
				t.Errorf("script lacks %q", want)
			}
		}
	})

	t.Run("the macOS leg asserts the mode from the tab's own view and stages the wake and claim records", func(t *testing.T) {
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if !strings.Contains(src, `assert-own-delivery-mode "$home/.codex/sessions"`) {
			t.Error("the macOS leg must assert delivery_mode from the tab's own rollout")
		}
		if !strings.Contains(src, `"$state/codex-wakes" "$state/codex-claims"`) {
			t.Error("the artifact must carry the wake generation and claim records")
		}
	})

	t.Run("workflow ships a binary per runner OS and gives the macOS leg a short home", func(t *testing.T) {
		b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "harness-compat.yml"))
		if err != nil {
			t.Fatal(err)
		}
		w := string(b)
		jobs := workflowJobs(w)
		if !strings.Contains(jobs["build"], "crosstalk-bin-${{ matrix.os }}") || !strings.Contains(jobs["build"], "macos-latest") {
			t.Error("build must publish a binary per runner OS, including macOS")
		}
		if !strings.Contains(jobs["harness"], "crosstalk-bin-${{ matrix.runner }}") {
			t.Error("a leg must download the binary built for its own runner")
		}
		if strings.Contains(w, "name: crosstalk-bin\n") {
			t.Error("no OS-less crosstalk-bin artifact may remain")
		}
		if strings.Count(w, "mktemp -d /tmp/ct.XXXXXX") < 2 {
			t.Error("legs must use a short scratch home so a unix socket path fits on macOS")
		}
	})
}

func TestHarnessCompatPtyrunMatchesTheRealRedrawnDialog(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	root := releaseRepoRoot(t)
	ptyrun := filepath.Join(root, "scripts", "ptyrun.py")
	fixture := filepath.Join(root, "scripts", "testdata", "codex-hooks-dialog.bin")
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	// The captured stream (Codex 0.167.0 on a hosted macOS runner) is the reason for the screen model: the dialog
	// text is never contiguous in the bytes, because the TUI redraws only the cells that changed.
	if bytes.Contains(raw, []byte("Hooks need review")) {
		t.Fatal("the fixture no longer shows the redraw problem it exists to pin")
	}
	if out, err := exec.Command("python3", "-I", ptyrun, "--match", fixture).CombinedOutput(); err != nil || !strings.Contains(string(out), "Hooks need review") {
		t.Fatalf("the real captured stream must match the trigger: %v\n%s", err, out)
	}
	neg := filepath.Join(t.TempDir(), "neg.bin")
	for name, body := range map[string][]byte{
		"unrelated screen":         []byte("\x1b[2;3Hloading\x1b[3;3HAsk Codex to do anything"),
		"dialog text erased again": append(append([]byte{}, raw...), []byte("\x1b[2J")...),
		"only half of the dialog":  []byte("\x1b[2;3HHooks "),
	} {
		if err := os.WriteFile(neg, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := exec.Command("python3", "-I", ptyrun, "--match", neg).Run(); err == nil {
			t.Errorf("%s must not match the trigger", name)
		}
	}
}

func TestHarnessCompatMacosLegRunsHookBypassOnlyInAScratchHome(t *testing.T) {
	root := releaseRepoRoot(t)
	script := filepath.Join(root, "scripts", "harness-compat-live.sh")
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "--dangerously-bypass-hook-trust"); n != 1 {
		t.Fatalf("the hook-trust bypass must appear exactly once, in the macOS tab launch; found %d", n)
	}
	run := func(home, logs string) (string, error) {
		cmd := exec.Command(script, "leg", "codex-macos", home)
		cmd.Env = append(os.Environ(), "OPENROUTER_API_KEY=dummy", "CLAUDE_MODEL=x", "CODEX_MODEL=y", "CROSSTALK_BIN=/nonexistent", "LIVE_LOGS="+logs)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	entries := func(dir string) []string {
		var names []string
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && p != dir {
				names = append(names, p)
			}
			return nil
		})
		return names
	}

	t.Run("a real home is refused before anything is created, installed or copied", func(t *testing.T) {
		home := notTmpDir(t, "notscratch")
		sentinel := filepath.Join(home, ".codex", "sessions", "2026", "rollout-private.jsonl")
		if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sentinel, []byte("private transcript"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := entries(home)
		logs := t.TempDir()
		out, err := run(home, logs)
		if err == nil || !strings.Contains(out, "scratch directory under /tmp") {
			t.Fatalf("a home outside /tmp must be refused: %v\n%s", err, out)
		}
		if after := entries(home); strings.Join(after, "\n") != strings.Join(before, "\n") {
			t.Errorf("the refused home was modified: before %v after %v", before, after)
		}
		if got := entries(logs); len(got) != 0 {
			t.Errorf("a refused home must not be copied into the logs: %v", got)
		}
	})

	t.Run("an empty home outside /tmp is refused by the /tmp rule alone", func(t *testing.T) {
		home := notTmpDir(t, "emptyreal")
		logs := t.TempDir()
		out, err := run(home, logs)
		if err == nil || !strings.Contains(out, "scratch directory under /tmp") {
			t.Fatalf("an empty home outside /tmp must be refused: %v\n%s", err, out)
		}
		if got := entries(home); len(got) != 0 {
			t.Errorf("the refused home was modified: %v", got)
		}
		if got := entries(logs); len(got) != 0 {
			t.Errorf("nothing may be copied into the logs: %v", got)
		}
	})

	t.Run("a scratch home that is not empty is refused, so a planted .codex link is never followed", func(t *testing.T) {
		home, err := os.MkdirTemp("/tmp", "ptyhome")
		if err != nil {
			t.Skip("no /tmp")
		}
		defer os.RemoveAll(home)
		external := t.TempDir()
		if err := os.Symlink(external, filepath.Join(home, ".codex")); err != nil {
			t.Fatal(err)
		}
		logs := t.TempDir()
		out, err := run(home, logs)
		if err == nil || !strings.Contains(out, "empty scratch home") {
			t.Fatalf("a non-empty home must be refused: %v\n%s", err, out)
		}
		if got := entries(external); len(got) != 0 {
			t.Errorf("the linked profile was touched: %v", got)
		}
		if got := entries(logs); len(got) != 0 {
			t.Errorf("nothing may be copied into the logs: %v", got)
		}
	})
}

func TestHarnessCompatLegNeverKeysOnModelProse(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		// The words the prompts use may appear only inside a prompt handed to a session, never in a check.
		for _, word := range []string{"READY", "PONG"} {
			if strings.Contains(l, word) && !strings.HasPrefix(l, "start_claude ") && !strings.HasPrefix(l, "start_codex ") && !strings.HasPrefix(l, "start_codex_nd ") && !strings.Contains(l, "compat-") {
				t.Errorf("line %d matches model prose (%s) outside a prompt: %s", i+1, word, l)
			}
		}
		// A queue-wake tab legitimately shows reachable false and an empty status, so no check may depend on them.
		if strings.Contains(l, "reachable") || strings.Contains(l, ".status") {
			t.Errorf("line %d depends on a reachable or status field: %s", i+1, l)
		}
	}
	if !strings.Contains(string(b), "waiting for a task_complete event in the rollout of thread") {
		t.Error("await_ready's timeout must name what it waited for")
	}
}
