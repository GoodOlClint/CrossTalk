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
	if want := `[{"leg":"codex-linux","harness":"codex","version":"0.171.0"},{"leg":"claude","harness":"claude-code","version":"2.1.330"}]`; legs != want {
		t.Fatalf("legs = %s, want %s", legs, want)
	}
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
			`{"claude-code":{"version":"2.1.330","legs":{"claude":"pass"},"pass":true},"codex":{"version":"0.171.0","legs":{"codex-linux":"fail"},"pass":false},"cross":"skipped"}`},
		{"missing key skips every step and is not a pass",
			[]map[string]any{job("harness-claude", "success", skipped), job("harness-codex-linux", "success", skipped), job("cross", "success", crossSkipped)},
			`{"claude-code":{"version":"2.1.330","legs":{"claude":"skipped"},"pass":false},"codex":{"version":"0.171.0","legs":{"codex-linux":"skipped"},"pass":false},"cross":"skipped"}`},
		{"success job whose test step did not succeed is not a pass",
			[]map[string]any{job("harness-claude", "success", map[string]string{"Run the leg": "failure"}), job("harness-codex-linux", "success", ran), job("cross", "success", crossRan)},
			`{"claude-code":{"version":"2.1.330","legs":{"claude":"fail"},"pass":false},"codex":{"version":"0.171.0","legs":{"codex-linux":"pass"},"pass":true},"cross":"pass"}`},
		{"a leg with no job reads as fail",
			[]map[string]any{job("harness-codex-linux", "success", ran)},
			`{"claude-code":{"version":"2.1.330","legs":{"claude":"fail"},"pass":false},"codex":{"version":"0.171.0","legs":{"codex-linux":"pass"},"pass":true},"cross":"fail"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := run("report", write(c.jobs...), legs); got != c.want {
				t.Fatalf("report = %s\nwant     %s", got, c.want)
			}
		})
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
