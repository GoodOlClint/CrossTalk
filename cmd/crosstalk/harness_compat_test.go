package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness/pi"
)

func compatDetect(t *testing.T, npmVersion string, args ...string) (string, error) {
	t.Helper()
	root := releaseRepoRoot(t)
	bin := t.TempDir()
	fake := "#!/bin/sh\necho " + npmVersion + "\n"
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(bin, "record.json")
	if err := os.WriteFile(rec, []byte(`{"codex": {"tested": "unverified", "checked": "2026-11-20"}, "claude-code": {"tested": "unverified", "checked": "2026-11-20"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(root, "scripts", "harness-compat.sh"), append([]string{"check-updates", rec}, args...)...)
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
	jobs := workflowJobs(w)
	secretJobs := map[string]bool{"harness": true, "report": true, "bump": true}
	for name, body := range jobs {
		has := strings.Contains(body, "secrets.") || strings.Contains(body, "environment:")
		if has != secretJobs[name] {
			t.Errorf("job %s: secrets or environment = %v, want %v", name, has, secretJobs[name])
		}
		if strings.Contains(body, "echo \"$OPENROUTER_API_KEY\"") || strings.Contains(body, "set -x") {
			t.Errorf("job %s must not echo the key", name)
		}
	}
	body := jobs["harness"]
	for _, want := range []string{"environment: harness-compat\n", "secrets.OPENROUTER_API_KEY", "HARNESS_COMPAT_CODEX_MODEL", "HARNESS_COMPAT_CLAUDE_MODEL", "HARNESS_COMPAT_OPENCODE_MODEL", "@opencode/cli@$VERSION", "HARNESS_COMPAT_PI_MODEL", "@earendil-works/pi-coding-agent@$VERSION", "fail-fast: false"} {
		if !strings.Contains(body, want) {
			t.Errorf("harness lacks %q", want)
		}
	}
	if !strings.Contains(jobs["report"], "environment: harness-compat\n") || !strings.Contains(jobs["report"], "secrets.OPENROUTER_API_KEY") {
		t.Error("report holds the key for the one usage read")
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

func TestHarnessCompatCompareCodexFailsClosed(t *testing.T) {
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
			cmd := exec.Command(filepath.Join(root, "scripts", "harness-compat.sh"), "compare-codex", home)
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

func TestHarnessCompatCompareClaudeSocketPath(t *testing.T) {
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
			cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), "compare-claude", home)
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
	toolSearch := func() string {
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "content": []map[string]any{{"type": "tool_reference", "tool_name": "x"}}}}}})
		return string(line) + "\n"
	}
	cases := []struct {
		name, log string
		ok        bool
	}{
		{"listed and bound", row("abc", true), true},
		{"array tool result after the listing", row("abc", true) + toolSearch(), true},
		{"array tool result only", toolSearch(), false},
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
	if want := `[{"leg":"codex-linux","harness":"codex","version":"0.171.0","runner":"ubuntu-latest"},{"leg":"codex-macos","harness":"codex","version":"0.171.0","runner":"macos-latest"},{"leg":"claude-linux","harness":"claude-code","version":"2.1.330","runner":"ubuntu-latest"},{"leg":"claude-macos","harness":"claude-code","version":"2.1.330","runner":"macos-latest"}]`; legs != want {
		t.Fatalf("legs = %s, want %s", legs, want)
	}
	rehearsal := run("legs", `[{"harness":"codex","version":"0.171.0"}]`, "0.167.0")
	if want := `[{"leg":"codex-linux","harness":"codex","version":"0.171.0","runner":"ubuntu-latest"},{"leg":"codex-macos","harness":"codex","version":"0.167.0","runner":"macos-latest"}]`; rehearsal != want {
		t.Fatalf("legs with a pinned queue version = %s, want %s", rehearsal, want)
	}
	if onlyClaude := run("legs", `[{"harness":"claude-code","version":"2.1.330"}]`, "0.167.0"); strings.Contains(onlyClaude, "codex") {
		t.Fatalf("a Claude-only change must not run a Codex leg: %s", onlyClaude)
	}
	// The report cases below use two legs so each fixture stays small.
	legs = `[{"leg":"codex-linux","harness":"codex","version":"0.171.0"},{"leg":"claude-linux","harness":"claude-code","version":"2.1.330"}]`
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
	cases := []struct {
		name string
		jobs []map[string]any
		want string
	}{
		{"claude passes, codex fails",
			[]map[string]any{job("harness-claude-linux", "success", ran), job("harness-codex-linux", "failure", map[string]string{"Run the leg": "failure"})},
			`{"claude-code":{"version":"2.1.330","legs":{"claude-linux":"pass"},"leg_versions":{"claude-linux":"2.1.330"},"pass":true},"codex":{"version":"0.171.0","legs":{"codex-linux":"fail"},"leg_versions":{"codex-linux":"0.171.0"},"pass":false}}`},
		{"missing key skips every step and is not a pass",
			[]map[string]any{job("harness-claude-linux", "success", skipped), job("harness-codex-linux", "success", skipped)},
			`{"claude-code":{"version":"2.1.330","legs":{"claude-linux":"skipped"},"leg_versions":{"claude-linux":"2.1.330"},"pass":false},"codex":{"version":"0.171.0","legs":{"codex-linux":"skipped"},"leg_versions":{"codex-linux":"0.171.0"},"pass":false}}`},
		{"success job whose test step did not succeed is not a pass",
			[]map[string]any{job("harness-claude-linux", "success", map[string]string{"Run the leg": "failure"}), job("harness-codex-linux", "success", ran)},
			`{"claude-code":{"version":"2.1.330","legs":{"claude-linux":"fail"},"leg_versions":{"claude-linux":"2.1.330"},"pass":false},"codex":{"version":"0.171.0","legs":{"codex-linux":"pass"},"leg_versions":{"codex-linux":"0.171.0"},"pass":true}}`},
		{"a leg with no job reads as fail",
			[]map[string]any{job("harness-codex-linux", "success", ran)},
			`{"claude-code":{"version":"2.1.330","legs":{"claude-linux":"fail"},"leg_versions":{"claude-linux":"2.1.330"},"pass":false},"codex":{"version":"0.171.0","legs":{"codex-linux":"pass"},"leg_versions":{"codex-linux":"0.171.0"},"pass":true}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := run("report", write(c.jobs...), legs); got != c.want {
				t.Fatalf("report = %s\nwant     %s", got, c.want)
			}
		})
	}
	// A rehearsal override that runs the queue leg on another Codex version does not make the candidate pass.
	pinned := `[{"leg":"codex-linux","harness":"codex","version":"0.171.0"},{"leg":"codex-macos","harness":"codex","version":"0.167.0"}]`
	allRan := write(job("harness-codex-linux", "success", ran), job("harness-codex-macos", "success", ran))
	if got := run("report", allRan, pinned); !strings.Contains(got, `"pass":false`) || !strings.Contains(got, `"codex-macos":"0.167.0"`) {
		t.Fatalf("a queue leg on another version must not pass the candidate: %s", got)
	}
	same := `[{"leg":"codex-linux","harness":"codex","version":"0.171.0"},{"leg":"codex-macos","harness":"codex","version":"0.171.0"}]`
	if got := run("report", allRan, same); !strings.Contains(got, `"pass":true`) {
		t.Fatalf("both Codex legs on the candidate version pass it: %s", got)
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
		cmd.Env = append(append(env, "HOME="+home, "PTYRUN_ANSWER_HOOKS=1"), extraEnv...)
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
	refused("answering is opt-in (daemon legs never set PTYRUN_ANSWER_HOOKS)", good, good, "CODEX_HOME="+filepath.Join(good, ".codex"), "PTYRUN_ANSWER_HOOKS=0")
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

func TestHarnessCompatAssertMailbox(t *testing.T) {
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
}

func compatLegArm(t *testing.T, src, arm string) string {
	t.Helper()
	i := strings.Index(src, "\n\t"+arm+")")
	if i < 0 {
		t.Fatalf("no %s leg arm", arm)
	}
	rest := src[i+1:]
	if j := strings.Index(rest[1:], "\n\t\t;;"); j >= 0 {
		rest = rest[:j+1]
	}
	return rest
}

func TestHarnessCompatLegsCallTheOneRowCheckAndItIsFatal(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	fnBody := src[strings.Index(src, "\tcodex_roundtrip() {"):]
	fnBody = fnBody[:strings.Index(fnBody, "\n\t}")]
	if strings.Count(fnBody, `one_row_check "$recv"`) != 1 {
		t.Error(`codex_roundtrip must call one_row_check "$recv" exactly once`)
	}
	for _, arm := range []string{"codex-linux", "codex-macos"} {
		if strings.Count(compatLegArm(t, src, arm), "codex_roundtrip") != 1 {
			t.Errorf("%s must run the shared codex_roundtrip exactly once", arm)
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
			out, err := exec.Command(script, "check-staged", c.dir, "th1").CombinedOutput()
			if (err == nil) != c.ok {
				t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
			}
			if has := strings.Contains(string(out), "PASS staged:"); has != c.ok {
				t.Errorf("%s: PASS staged line present = %v, want %v (output %q)", name, has, c.ok, out)
			}
			if c.ok && (!strings.Contains(string(out), "codex-wakes/th1.json") || !strings.Contains(string(out), "codex-claims/")) {
				t.Errorf("%s: the PASS line must name the wake generation record and the claim record: %q", name, out)
			}
		}
	})

	t.Run("a successful macOS leg fails if its staged artifact lacks the records, and asks the tab for a fresh listing when needed", func(t *testing.T) {
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, want := range []string{`check-staged "$out/state" "$tab_ref"`, `[ "$staged" -eq 0 ] || exit 1`, `type_line 8 "Call the crosstalk list_agents tool once more."`} {
			if !strings.Contains(src, want) {
				t.Errorf("script lacks %q", want)
			}
		}
	})

	t.Run("check-staged runs on the staged copy, after staging, and a staging failure fails the leg", func(t *testing.T) {
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		stage := strings.Index(src, `"$0" stage-logs "$logs" "$out" || staged=1`)
		check := strings.Index(src, `check-staged "$out/state" "$tab_ref"`)
		if stage < 0 || check < 0 || stage > check {
			t.Errorf("finish must stage the logs (and count a staging failure) before checking the staged copy (stage %d, check %d)", stage, check)
		}
		if strings.Contains(src, `check-staged "$logs/state"`) {
			t.Error("check-staged must read the staged artifact, not the source files")
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
			if strings.Contains(l, word) && !strings.HasPrefix(l, "start_claude ") && !strings.HasPrefix(l, "start_codex ") && !strings.HasPrefix(l, "start_codex_nd ") && !strings.HasPrefix(l, "oc_prompt ") && !strings.HasPrefix(l, "start_pi ") && !strings.Contains(l, "compat-") {
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

func readCompatWorkflow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), ".github", "workflows", "harness-compat.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHarnessCompatRestructureJobs(t *testing.T) {
	w := readCompatWorkflow(t)
	jobs := workflowJobs(w)
	var names []string
	for n := range jobs {
		names = append(names, n)
	}
	want := map[string]bool{"check-updates": true, "compare": true, "build": true, "harness": true, "report": true, "bump": true, "issue": true}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected job %q: the workflow has check-updates, compare, build, harness, report, bump and issue only", n)
		}
	}
	for n := range want {
		if _, ok := jobs[n]; !ok {
			t.Errorf("missing job %q", n)
		}
	}
	for _, gone := range []string{"cross:", "detect:", "tier0", "key-before", "key-after"} {
		if strings.Contains(w, gone) {
			t.Errorf("the workflow must not mention %q", gone)
		}
	}
	if !strings.Contains(jobs["compare"], "name: compare-${{ matrix.leg }}") || !strings.Contains(jobs["harness"], "name: harness-${{ matrix.leg }}") {
		t.Error("compare and harness jobs are named from the leg: compare-<leg>, harness-<leg>")
	}
}

func TestHarnessCompatLegNamesAreTheSameHarnessLegs(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	out, err := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), "legs",
		`[{"harness":"codex","version":"0.171.0"},{"harness":"claude-code","version":"2.1.330"},{"harness":"opencode","version":"2.4.1"},{"harness":"pi","version":"1.6.0"}]`).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var legs []struct{ Leg, Runner string }
	if err := json.Unmarshal(out, &legs); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range legs {
		got[l.Leg] = l.Runner
	}
	want := map[string]string{"claude-linux": "ubuntu-latest", "claude-macos": "macos-latest", "codex-linux": "ubuntu-latest", "codex-macos": "macos-latest", "opencode-linux": "ubuntu-latest", "opencode-macos": "macos-latest", "pi-linux": "ubuntu-latest", "pi-macos": "macos-latest"}
	if len(got) != 8 {
		t.Fatalf("legs = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("leg %s runner = %q, want %q", k, got[k], v)
		}
	}
}

func TestHarnessCompatUsageIsReadOnceInReport(t *testing.T) {
	w := readCompatWorkflow(t)
	if n := strings.Count(w, "key-usage"); n != 1 {
		t.Fatalf("OpenRouter usage must be read in exactly one place, found %d reads", n)
	}
	if !strings.Contains(workflowJobs(w)["report"], "key-usage") {
		t.Error("the one usage read belongs to the report job")
	}
}

func TestHarnessCompatNoGateOutputs(t *testing.T) {
	w := readCompatWorkflow(t)
	for _, bad := range []string{"ran=false", "ran=true", "steps.gate.outputs", "ok=false", "steps.prereq.outputs"} {
		if strings.Contains(w, bad) {
			t.Errorf("a job must not gate by an output that later steps skip on (%q): use a job-level if or a failing step", bad)
		}
	}
	h := workflowJobs(w)["harness"]
	for _, want := range []string{"exit 1", "blocked"} {
		if !strings.Contains(h, want) {
			t.Errorf("harness must fail (exit 1, %q) when its compare job did not pass", want)
		}
	}
	if !strings.Contains(h, "OPENROUTER_API_KEY") || !strings.Contains(h, `[ -n "$OPENROUTER_API_KEY" ]`) {
		t.Error("harness must fail on a missing key, not skip")
	}
}

func TestHarnessCompatEveryLegAssertsTheScratchHomeDescriptor(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	_, piBody, _ := strings.Cut(src, "\tpi_roundtrip() {")
	piBody, _, _ = strings.Cut(piBody, "\n\t}")
	if strings.Count(piBody, "assert_home_resolved pi") != 1 {
		t.Error("the shared Pi round trip must run assert_home_resolved pi exactly once")
	}
	if strings.Count(compatLegArm(t, src, "claude-linux|claude-macos"), "assert_home_resolved claude") != 1 {
		t.Error("the Claude legs must run assert_home_resolved claude exactly once")
	}
	fnBody := src[strings.Index(src, "\tcodex_roundtrip() {"):]
	fnBody = fnBody[:strings.Index(fnBody, "\n\t}")]
	if strings.Count(fnBody, "assert_home_resolved codex") != 1 {
		t.Error("the shared Codex round trip must run assert_home_resolved codex exactly once")
	}
	if strings.Count(compatLegArm(t, src, "codex-macos"), "assert_home_resolved codex") != 1 {
		t.Error("the macOS queue phase must also run assert_home_resolved codex exactly once")
	}
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "assert-home-descriptor") && strings.Contains(line, "||") {
			t.Errorf("the scratch-HOME check must stay fatal: %q", strings.TrimSpace(line))
		}
	}
}

func TestHarnessCompatAssertHomeDescriptor(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	script := filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh")
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(harness string) error {
		return exec.Command(script, "assert-home-descriptor", state, harness).Run()
	}
	if run("claude") == nil {
		t.Fatal("no descriptor in the scratch home must fail")
	}
	if err := os.WriteFile(filepath.Join(state, "agents", "a.json"), []byte(`{"agent_id":"a","harness":"codex"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if run("claude") == nil {
		t.Fatal("a descriptor of another harness must fail")
	}
	if err := run("codex"); err != nil {
		t.Fatalf("a codex descriptor under the scratch state must pass: %v", err)
	}
}

func TestHarnessCompatOnlyTheQueuePhaseAnswersHookDialogs(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	fn := func(name string) string {
		i := strings.Index(src, "\t"+name+"() {")
		if i < 0 {
			t.Fatalf("no %s", name)
		}
		body := src[i:]
		return body[:strings.Index(body, "\n\t}")]
	}
	if strings.Contains(fn("start_codex"), "PTYRUN_ANSWER_HOOKS") {
		t.Error("the daemon launcher must leave the hook-review dialog unanswered")
	}
	if !strings.Contains(fn("start_codex_nd"), "PTYRUN_ANSWER_HOOKS=1") {
		t.Error("the no-daemon queue launcher opts in to answering the hook-review dialog")
	}
}

func TestHarnessCompatLegNeedsOnlyItsOwnHarnessModel(t *testing.T) {
	script := filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh")
	for leg, c := range map[string]struct{ set, unset string }{
		"claude-linux":   {"CLAUDE_MODEL", "CODEX_MODEL"},
		"claude-macos":   {"CLAUDE_MODEL", "CODEX_MODEL"},
		"codex-linux":    {"CODEX_MODEL", "CLAUDE_MODEL"},
		"codex-macos":    {"CODEX_MODEL", "CLAUDE_MODEL"},
		"opencode-linux": {"OPENCODE_MODEL", "CODEX_MODEL"},
		"opencode-macos": {"OPENCODE_MODEL", "CLAUDE_MODEL"},
		"pi-linux":       {"PI_COMPAT_MODEL", "CODEX_MODEL"},
		"pi-macos":       {"PI_COMPAT_MODEL", "OPENCODE_MODEL"},
	} {
		cmd := exec.Command(script, "leg", leg, t.TempDir())
		var env []string
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "CLAUDE_MODEL=") && !strings.HasPrefix(kv, "CODEX_MODEL=") && !strings.HasPrefix(kv, "OPENCODE_MODEL=") && !strings.HasPrefix(kv, "PI_COMPAT_MODEL=") && !strings.HasPrefix(kv, "CROSSTALK_BIN=") && !strings.HasPrefix(kv, "LIVE_LOGS=") {
				env = append(env, kv)
			}
		}
		cmd.Env = append(env, "OPENROUTER_API_KEY=k", c.set+"=m")
		out, _ := cmd.CombinedOutput()
		if strings.Contains(string(out), c.unset) {
			t.Errorf("%s must not need %s:\n%s", leg, c.unset, out)
		}
	}
}

func TestHarnessCompatInterruptedCase(t *testing.T) {
	root := releaseRepoRoot(t)
	script := filepath.Join(root, "scripts", "harness-compat-live.sh")

	t.Run("turns counts task_started, task_complete and turn_aborted per session rollout", func(t *testing.T) {
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("python3 not on PATH")
		}
		ev := func(kind string) string {
			b, _ := json.Marshal(map[string]any{"payload": map[string]any{"type": kind}})
			return string(b) + "\n"
		}
		dir := filepath.Join(t.TempDir(), "2026", "11", "21")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{
			"th1":   ev("task_started") + ev("task_complete") + ev("task_started") + ev("turn_aborted") + "not json\n",
			"other": ev("task_started") + ev("task_started"),
		}
		for ref, body := range files {
			if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-21T01-00-00-"+ref+".jsonl"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		out, err := exec.Command(script, "turns", filepath.Dir(filepath.Dir(filepath.Dir(dir))), "th1").Output()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(out)); got != "2 1 1" {
			t.Errorf("turns = %q, want %q (started, completed, aborted)", got, "2 1 1")
		}
	})

	t.Run("events lists turn starts as queue or user, completions and aborts, in order", func(t *testing.T) {
		if _, err := exec.LookPath("python3"); err != nil {
			t.Skip("python3 not on PATH")
		}
		started := func(trigger string) string {
			p := map[string]any{"type": "task_started"}
			if trigger != "" {
				p["turn_attribution"] = map[string]any{"turn_trigger": trigger}
			}
			b, _ := json.Marshal(map[string]any{"payload": p})
			return string(b) + "\n"
		}
		ev := func(kind string) string {
			b, _ := json.Marshal(map[string]any{"payload": map[string]any{"type": kind}})
			return string(b) + "\n"
		}
		for name, c := range map[string]struct{ body, want string }{
			"operator then wake":             {started("") + ev("task_complete") + started("queue") + ev("task_complete"), "user done queue done"},
			"wake first":                     {started("queue") + ev("task_complete") + started(""), "queue done user"},
			"operator aborted, wake follows": {started("") + ev("turn_aborted") + started("queue"), "user abort queue"},
		} {
			dir := filepath.Join(t.TempDir(), "2026", "11", "21")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "rollout-2026-11-21T01-00-00-th1.jsonl"), []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(script, "events", filepath.Dir(filepath.Dir(filepath.Dir(dir))), "th1").Output()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(strings.Fields(string(out)), " "); got != c.want {
				t.Errorf("%s: events = %q, want %q", name, got, c.want)
			}
		}
	})

	t.Run("the pause check matches the wake's own row by client_id, not any row", func(t *testing.T) {
		if _, err := exec.LookPath("sqlite3"); err != nil {
			t.Skip("sqlite3 not on PATH")
		}
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(`"(select count\(\*\) from queued_items where json_extract[^"]*)"`).FindStringSubmatch(string(b))
		if m == nil {
			t.Fatal("script has no row-identity query")
		}
		q := strings.NewReplacer("$wake_id", "wake-1", `\$`, "$").Replace(m[1])
		db := filepath.Join(t.TempDir(), "q.sqlite")
		setup := `create table queued_items (id text, thread_id text, payload_json text); insert into queued_items values ('a','th1','{"UserInput":{"client_id":"other"}}');`
		if out, err := exec.Command("sqlite3", db, setup).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		count := func() string {
			out, err := exec.Command("sqlite3", "-readonly", db, q).CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			return strings.TrimSpace(string(out))
		}
		if got := count(); got != "0" {
			t.Errorf("an unrelated row counted: %s", got)
		}
		if out, err := exec.Command("sqlite3", db, `insert into queued_items values ('b','th1','{"UserInput":{"client_id":"wake-1"}}')`).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if got := count(); got != "1" {
			t.Errorf("the wake row did not count: %s", got)
		}
	})

	t.Run("wake-state reads the generation record", func(t *testing.T) {
		d := t.TempDir()
		if err := os.MkdirAll(filepath.Join(d, "codex-wakes"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "codex-wakes", "th1.json"), []byte(`{"state":"queued"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		for ref, want := range map[string]string{"th1": "queued", "none": "missing"} {
			out, _ := exec.Command(script, "wake-state", d, ref).Output()
			if got := strings.TrimSpace(string(out)); got != want {
				t.Errorf("wake-state %s = %q, want %q", ref, got, want)
			}
		}
	})

	t.Run("the macOS leg runs the interrupted case after the wake check and fails on any violation", func(t *testing.T) {
		b, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		wake := strings.Index(src, `"$0" assert-wake "$state" "$tab_ref"`)
		call := strings.Index(src, `interrupted_case "$tab_ref" "$codex_id"`)
		if wake < 0 || call < wake {
			t.Error("the macOS leg must call interrupted_case after assert-wake")
		}
		if strings.Contains(src, `'%s\r'`) {
			t.Error("typed lines must send Enter as its own write after the text (type_line), not appended to it")
		}
		if strings.Index(src, "type_line() {") > strings.Index(src, "interrupted_case() {") {
			t.Error("type_line must be defined with the leg helpers, before interrupted_case and the delivery-mode retry")
		}
		for _, want := range []string{
			`printf '\033' >&8`,
			`type_line 8 "Run the shell command: sleep 120.`,
			`type_line 9 "Use the crosstalk send_message tool`,
			`type_line 8 "Say one word."`,
			`tab rollout events (last 12):`,
			`PASS interrupted: a long turn was running and a wake row was queued behind it`,
			`PASS interrupted: after Esc the turn aborted, and for 45 s no turn started, the wake generation stayed queued and the queue row stayed`,
			`PASS interrupted: the next prompt ran, then the wake dispatched (generation collected)`,
			`fail "a turn started while the thread was interrupted`,
			`from queued_items`,
			`"$0" events "$home/.codex/sessions" "$iref"`,
			`"user done queue"`,
			`the tab's earlier turn never finished`,
		} {
			if !strings.Contains(src, want) {
				t.Errorf("script lacks %q", want)
			}
		}
	})
}

func TestHarnessCompatQueueVersionDefaultsToTheCandidate(t *testing.T) {
	w := readCompatWorkflow(t)
	i := strings.Index(w, "queue_codex_version:")
	if i < 0 {
		t.Fatal("workflow lacks the queue_codex_version input")
	}
	block := w[i:]
	block = block[:strings.Index(block, "\npermissions:")]
	if !strings.Contains(block, `default: ""`) {
		t.Errorf("queue_codex_version must default to empty so codex-macos runs the candidate:\n%s", block)
	}
	for _, gone := range []string{"0.167", "allows 0.167.x only"} {
		if strings.Contains(block, gone) {
			t.Errorf("the input must not name %q:\n%s", gone, block)
		}
	}
}

func TestHarnessCompatReportJobFailsWhenAHarnessDidNotPass(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	w := readCompatWorkflow(t)
	report := workflowJobs(w)["report"]
	write := strings.Index(report, `echo "results=$results" >> "$GITHUB_OUTPUT"`)
	usage := strings.Index(report, "key-usage now")
	check := strings.Index(report, "all(.[]; .pass)")
	if write < 0 || usage < 0 || check < 0 || !(write < usage && usage < check) {
		t.Fatalf("the report job must write its results output and usage line before a final step that fails on a non-pass (write %d, usage %d, check %d)", write, usage, check)
	}
	if !strings.Contains(report, "exit 1") {
		t.Error("the report job's final step must exit 1")
	}
	if strings.Contains(report, "exit 0") {
		t.Error("the report job must not exit early with exit 0 before the failure check")
	}
	for results, want := range map[string]bool{
		`{"codex":{"pass":true},"claude-code":{"pass":true}}`:                           true,
		`{"codex":{"pass":true},"claude-code":{"pass":false}}`:                          false,
		`{"codex":{"pass":false}}`:                                                      false,
		`{"codex":{"pass":true},"claude-code":{"pass":true},"opencode":{"pass":true}}`:  true,
		`{"codex":{"pass":true},"claude-code":{"pass":true},"opencode":{"pass":false}}`: false,
		`{"opencode":{"pass":false}}`:                                                   false,
		`{"codex":{"pass":true},"pi":{"pass":true}}`:                                    true,
		`{"codex":{"pass":true},"pi":{"pass":false}}`:                                   false,
	} {
		cmd := exec.Command("sh", "-c", `printf '%s' "$R" | jq -e 'all(.[]; .pass)' >/dev/null`)
		cmd.Env = append(os.Environ(), "R="+results)
		if got := cmd.Run() == nil; got != want {
			t.Errorf("%s: check passed = %v, want %v", results, got, want)
		}
	}
}

func TestHarnessCompatScheduleAndNoPullRequestTrigger(t *testing.T) {
	w := readCompatWorkflow(t)
	on := w[:strings.Index(w, "\npermissions:")]
	if !regexp.MustCompile(`(?m)^  schedule:\n    - cron: "[^"]+"`).MatchString(on) {
		t.Errorf("workflow needs a schedule trigger with a cron:\n%s", on)
	}
	if !strings.Contains(on, "workflow_dispatch:") {
		t.Error("workflow_dispatch stays")
	}
	if strings.Contains(on, "pull_request") {
		t.Error("no pull_request or pull_request_target trigger")
	}
	if !strings.Contains(w, "HARNESS: ${{ inputs.harness || 'all' }}") {
		t.Error("a scheduled run has no inputs, so the harness must default to all")
	}
}

func TestHarnessCompatBumpAndIssueKeyOnReportResults(t *testing.T) {
	jobs := workflowJobs(readCompatWorkflow(t))
	for _, name := range []string{"bump", "issue"} {
		body := jobs[name]
		for _, want := range []string{"needs: [check-updates, report]", "if: always() &&", "needs.report.outputs.results"} {
			if !strings.Contains(body, want) {
				t.Errorf("job %s lacks %q", name, want)
			}
		}
		for _, bad := range []string{"download-artifact", "npm install", "needs.harness", "matrix.", "OPENROUTER", "go test", "go build"} {
			if strings.Contains(body, bad) {
				t.Errorf("job %s must not contain %q: it runs no harness code and reads no runner-written file", name, bad)
			}
		}
	}
	bump := jobs["bump"]
	for _, want := range []string{"environment: harness-compat-bump\n", "actions/create-github-app-token@v3", "land-bump", "persist-credentials: false", `"$h" "$v" "$GITHUB_SHA"`} {
		if !strings.Contains(bump, want) {
			t.Errorf("bump lacks %q", want)
		}
	}
	if strings.Contains(jobs["issue"], "environment:") || strings.Contains(jobs["issue"], "create-github-app-token") || !strings.Contains(jobs["issue"], "issues: write") {
		t.Error("issue runs on GITHUB_TOKEN with issues: write and no environment or App key")
	}
	for name, body := range jobs {
		if name != "bump" && (strings.Contains(body, "RELEASE_APP") || strings.Contains(body, "app.outputs.token")) {
			t.Errorf("job %s must not hold the App key", name)
		}
	}
}

const compatResults = `{"codex":{"version":"0.172.0","pass":true,"legs":{}},"claude-code":{"version":"2.1.330","pass":false,"legs":{}}}`

func TestHarnessCompatPassingFailingAndBumpRecord(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	root := releaseRepoRoot(t)
	sc := filepath.Join(root, "scripts", "harness-compat.sh")
	run := func(args ...string) (string, error) {
		out, err := exec.Command(sc, args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run("passing", compatResults); err != nil || out != "codex 0.172.0" {
		t.Errorf("passing = %q, %v", out, err)
	}
	if out, err := run("failing", compatResults); err != nil || out != "claude-code 2.1.330" {
		t.Errorf("failing = %q, %v", out, err)
	}
	rec := filepath.Join(t.TempDir(), "rec.json")
	orig := `{
  "codex": {"tested": "unverified", "checked": "2026-11-20"},
  "claude-code": {"tested": "2.1.329", "checked": "2026-11-20"}
}
`
	if err := os.WriteFile(rec, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run("bump-record", rec, "codex", "0.172.0", "2026-11-21"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got, _ := os.ReadFile(rec)
	want := strings.Replace(orig, `"codex": {"tested": "unverified", "checked": "2026-11-20"}`, `"codex": {"tested": "0.172.0", "checked": "2026-11-21"}`, 1)
	if string(got) != want {
		t.Errorf("record changed more than the codex entry:\n%s", got)
	}
	for _, bad := range [][]string{
		{"bump-record", rec, "cursor", "1.2.3", "2026-11-21"},
		{"bump-record", rec, "codex", "1.2.3; rm -rf /", "2026-11-21"},
		{"bump-record", rec, "codex", "1.2.3", "yesterday"},
	} {
		if out, err := run(bad...); err == nil {
			t.Errorf("%v accepted: %s", bad, out)
		}
	}
	if after, _ := os.ReadFile(rec); string(after) != want {
		t.Error("a refused bump changed the record")
	}
}

const fakeIssueGH = `#!/bin/sh
echo "gh $*" >> "$FAKE_DIR/log"
case "$1 $2" in
"issue list")
	[ -e "$FAKE_DIR/listfail" ] && { echo "HTTP 502" >&2; exit 1; }
	cat "$FAKE_DIR/open_issues" 2>/dev/null || echo '[]'
	;;
"issue create")
	printf '%s\n' "$*" >> "$FAKE_DIR/created"
	echo "https://example.test/issues/9"
	[ -e "$FAKE_DIR/nolabel" ] && case "$*" in *--label*) echo "could not add label" >&2; exit 1 ;; esac
	;;
esac
exit 0
`

func fileIssue(t *testing.T, fake, bin, jobs string) (string, error) {
	t.Helper()
	cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), "file-issue", "Owner/Repo", "claude-code", "2.1.330", "https://github.com/Owner/Repo/actions/runs/7", jobs)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_DIR="+fake)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestHarnessCompatIssueIsReusedAndHasNoIdentifyingText(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	fake, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeIssueGH), 0o755); err != nil {
		t.Fatal(err)
	}
	jobs := filepath.Join(fake, "jobs.json")
	jobsJSON := `{"name":"harness-claude-macos","conclusion":"failure","steps":[{"name":"Run the leg","conclusion":"failure"}]}
{"name":"harness-codex-linux","conclusion":"failure","steps":[{"name":"Run the leg","conclusion":"failure"}]}
`
	if err := os.WriteFile(jobs, []byte(jobsJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := fileIssue(t, fake, bin, jobs); err != nil {
		t.Fatalf("first failure: %v\n%s", err, out)
	}
	created, _ := os.ReadFile(filepath.Join(fake, "created"))
	c := string(created)
	for _, want := range []string{"--label harness-compat", "harness-compat: claude-code 2.1.330 did not pass", "version 2.1.330", "harness-claude-macos: Run the leg", "https://github.com/Owner/Repo/actions/runs/7"} {
		if !strings.Contains(c, want) {
			t.Errorf("issue lacks %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "codex") && strings.Contains(c, "harness-codex-linux") {
		t.Errorf("a claude-code issue lists a codex job:\n%s", c)
	}
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	for _, id := range []string{home, os.Getenv("USER"), host, "/Users/", "/home/"} {
		if id != "" && strings.Contains(c, id) {
			t.Errorf("issue text contains identifying %q:\n%s", id, c)
		}
	}
	if err := os.WriteFile(filepath.Join(fake, "open_issues"), []byte(`[{"number":9,"title":"harness-compat: claude-code 2.1.330 did not pass"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := fileIssue(t, fake, bin, jobs); err != nil || !strings.Contains(out, "reusing issue #9") {
		t.Fatalf("second failure: %v\n%s", err, out)
	}
	created, _ = os.ReadFile(filepath.Join(fake, "created"))
	if n := strings.Count(string(created), "issue create"); n != 1 {
		t.Errorf("issue create ran %d times across two failures, want 1", n)
	}
	if err := os.WriteFile(filepath.Join(fake, "listfail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(fake, "created"))
	if out, err := fileIssue(t, fake, bin, jobs); err == nil {
		t.Fatalf("a failed issue lookup must not file an issue:\n%s", out)
	}
	if after, _ := os.ReadFile(filepath.Join(fake, "created")); string(after) != string(before) {
		t.Error("issue create ran after a failed lookup")
	}
	if err := os.Remove(filepath.Join(fake, "listfail")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, "open_issues"), []byte(`[{"number":4,"title":"harness-compat: claude-code 2.1.329 did not pass"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := fileIssue(t, fake, bin, jobs); err != nil {
		t.Fatalf("other version open: %v\n%s", err, out)
	}
	created, _ = os.ReadFile(filepath.Join(fake, "created"))
	if n := strings.Count(string(created), "issue create"); n != 2 {
		t.Errorf("an open issue for another version must not suppress this one; creates = %d", n)
	}
}

func TestHarnessCompatTestedCodeAllowsOnlyRecordChanges(t *testing.T) {
	git := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(dir, "init", "-q", "-b", "main")
	write("a.go", "1")
	write(".github/harness-compat.json", "{}")
	git(dir, "add", "-A")
	git(dir, "commit", "-qm", "tested")
	git(dir, "branch", "tested")
	write(".github/harness-compat.json", `{"codex":1}`)
	git(dir, "commit", "-qam", "record only")
	check := func() error {
		c := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), "tested-code", "tested", "main", ".github/harness-compat.json")
		c.Dir = dir
		out, err := c.CombinedOutput()
		if err != nil && !strings.Contains(string(out), "a.go") && strings.Contains(string(out), "changed since") {
			t.Errorf("a refusal must name the changed file: %s", out)
		}
		return err
	}
	if err := check(); err != nil {
		t.Fatalf("a record-only change must be allowed: %v", err)
	}
	write("a.go", "2")
	git(dir, "commit", "-qam", "adapter change")
	if err := check(); err == nil {
		t.Fatal("main moved on code after the tested revision: the bump must be refused")
	}
	c := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), "tested-code", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "main", ".github/harness-compat.json")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err == nil || !strings.Contains(string(out), "failed") {
		t.Fatalf("a git diff error must fail the check, got err=%v\n%s", err, out)
	}
}

func compatJobs(t *testing.T, jobs ...map[string]any) string {
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

func compatJob(name, conclusion string, steps ...string) map[string]any {
	var st []map[string]any
	for _, n := range steps {
		st = append(st, map[string]any{"name": n, "conclusion": "failure"})
	}
	return map[string]any{"name": name, "conclusion": conclusion, "steps": st}
}

func compatScript(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func TestHarnessCompatOpenCodeLegsAndReport(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := compatScript(t, args...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	only := run("legs", `[{"harness":"opencode","version":"2.4.1"}]`, "0.167.0")
	if want := `[{"leg":"opencode-linux","harness":"opencode","version":"2.4.1","runner":"ubuntu-latest"},{"leg":"opencode-macos","harness":"opencode","version":"2.4.1","runner":"macos-latest"}]`; only != want {
		t.Fatalf("opencode-only legs = %s, want %s (the Codex queue override must not touch it)", only, want)
	}
	mixed := run("legs", `[{"harness":"codex","version":"0.171.0"},{"harness":"claude-code","version":"2.1.330"},{"harness":"opencode","version":"2.4.1"}]`)
	var legs []struct{ Leg, Harness, Version, Runner string }
	if err := json.Unmarshal([]byte(mixed), &legs); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range legs {
		got[l.Leg] = l.Runner + " " + l.Harness + " " + l.Version
	}
	want := map[string]string{
		"codex-linux": "ubuntu-latest codex 0.171.0", "codex-macos": "macos-latest codex 0.171.0",
		"claude-linux": "ubuntu-latest claude-code 2.1.330", "claude-macos": "macos-latest claude-code 2.1.330",
		"opencode-linux": "ubuntu-latest opencode 2.4.1", "opencode-macos": "macos-latest opencode 2.4.1",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) || len(legs) != 6 {
		t.Fatalf("mixed legs = %v, want %v", got, want)
	}
	if noOC := run("legs", `[{"harness":"codex","version":"0.171.0"}]`); strings.Contains(noOC, "opencode") {
		t.Fatalf("a Codex-only change must not run an OpenCode leg: %s", noOC)
	}

	ocLegs := only
	cases := []struct {
		name, legs string
		jobs       []map[string]any
		wantPass   map[string]bool
	}{
		{"opencode-only pass", ocLegs,
			[]map[string]any{passJob("harness-opencode-linux"), passJob("harness-opencode-macos")},
			map[string]bool{"opencode": true}},
		{"opencode fails while codex and claude pass",
			run("legs", `[{"harness":"codex","version":"0.171.0"},{"harness":"claude-code","version":"2.1.330"},{"harness":"opencode","version":"2.4.1"}]`),
			[]map[string]any{passJob("harness-codex-linux"), passJob("harness-codex-macos"), passJob("harness-claude-linux"), passJob("harness-claude-macos"),
				passJob("harness-opencode-linux"), compatJob("harness-opencode-macos", "failure", "Run the leg")},
			map[string]bool{"codex": true, "claude-code": true, "opencode": false}},
		{"a changed harness with only its Linux result is not a pass", ocLegs,
			[]map[string]any{passJob("harness-opencode-linux")},
			map[string]bool{"opencode": false}},
		{"a changed harness with only its macOS result is not a pass", ocLegs,
			[]map[string]any{passJob("harness-opencode-macos")},
			map[string]bool{"opencode": false}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := run("report", compatJobs(t, c.jobs...), c.legs)
			var res map[string]struct{ Pass bool }
			if err := json.Unmarshal([]byte(out), &res); err != nil {
				t.Fatal(err)
			}
			if len(res) != len(c.wantPass) {
				t.Fatalf("report = %s, want harnesses %v", out, c.wantPass)
			}
			for h, p := range c.wantPass {
				if res[h].Pass != p {
					t.Errorf("%s pass = %v, want %v\n%s", h, res[h].Pass, p, out)
				}
			}
		})
	}
}

func passJob(name string) map[string]any {
	return map[string]any{"name": name, "conclusion": "success", "steps": []map[string]any{{"name": "Run the leg", "conclusion": "success"}}}
}

func TestHarnessCompatCheckUpdatesAllIncludesOpenCode(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	root := releaseRepoRoot(t)
	bin := t.TempDir()
	fake := "#!/bin/sh\necho \"$*\" >> \"$FAKE_LOG\"\ncase \"$2\" in @opencode/cli) echo 2.4.1 ;; @openai/codex) echo 0.171.0 ;; *) echo 2.1.330 ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(bin, "record.json")
	if err := os.WriteFile(rec, []byte(`{"codex": {"tested": "0.171.0", "checked": "2026-11-20"}, "claude-code": {"tested": "2.1.330", "checked": "2026-11-20"}, "pi": {"tested": "2.1.330", "checked": "2026-11-20"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	logf := filepath.Join(bin, "npm.log")
	run := func(args ...string) (string, error) {
		cmd := exec.Command(filepath.Join(root, "scripts", "harness-compat.sh"), append([]string{"check-updates", rec}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_LOG="+logf)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run("all", "", "", ""); err != nil || out != `[{"harness":"opencode","version":"2.4.1"}]` {
		t.Fatalf("all with codex and claude-code recorded: %q, %v (an unrecorded opencode reads as unverified)", out, err)
	}
	if b, _ := os.ReadFile(logf); !strings.Contains(string(b), "view @opencode/cli version") || strings.Contains(string(b), "opencode-ai") {
		t.Errorf("npm was asked for: %s", b)
	}
	if out, err := run("opencode", "2.4.1", "", "2.4.1"); err != nil || out != `[]` {
		t.Errorf("recorded equal: %q, %v", out, err)
	}
	if out, err := run("opencode", "2.4.1", "true", "2.4.1"); err != nil || out != `[{"harness":"opencode","version":"2.4.1"}]` {
		t.Errorf("forced: %q, %v", out, err)
	}
	if out, err := run("all", "2.4.1", "", ""); err == nil {
		t.Errorf("a forced version for all harnesses was accepted: %s", out)
	}
}

func TestHarnessCompatFailedStepsAttributesByHarness(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	jobs := compatJobs(t,
		compatJob("compare-claude-linux", "failure", "Harness probes"),
		compatJob("harness-claude-macos", "failure", "Run the leg"),
		compatJob("compare-codex-linux", "failure", "Harness probes"),
		compatJob("harness-codex-macos", "failure", "Run the leg"),
		compatJob("compare-opencode-linux", "failure", "Harness probes"),
		compatJob("harness-opencode-linux", "failure", "Run the leg"),
		compatJob("harness-opencode-macos", "success"),
		compatJob("compare-pi-linux", "failure", "Harness probes"),
		compatJob("harness-pi-macos", "failure", "Run the leg"),
		compatJob("build (ubuntu-latest)", "failure", "go vet"),
	)
	for h, own := range map[string]string{"opencode": "opencode", "codex": "codex", "claude-code": "claude", "pi": "pi"} {
		out, err := compatScript(t, "failed-steps", jobs, h)
		if err != nil {
			t.Fatalf("%s: %v\n%s", h, err, out)
		}
		for _, other := range []string{"claude", "codex", "opencode", "pi"} {
			has := strings.Contains(out, "-"+other+"-")
			if (other == own) != has {
				t.Errorf("%s: lists %s jobs = %v\n%s", h, other, has, out)
			}
		}
		if !strings.Contains(out, "build (ubuntu-latest): go vet") {
			t.Errorf("%s: a shared job failure is dropped:\n%s", h, out)
		}
	}
	if out, err := compatScript(t, "failed-steps", jobs, "cursor"); err == nil {
		t.Errorf("unknown harness accepted: %s", out)
	}
}

func TestHarnessCompatMixedPassFailBumpsAndFilesIssue(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	legs, err := compatScript(t, "legs", `[{"harness":"codex","version":"0.172.0"},{"harness":"opencode","version":"2.4.1"}]`)
	if err != nil {
		t.Fatal(legs)
	}
	jobs := compatJobs(t,
		passJob("harness-codex-linux"), passJob("harness-codex-macos"),
		passJob("harness-opencode-linux"), compatJob("harness-opencode-macos", "failure", "Run the leg"))
	results, err := compatScript(t, "report", jobs, legs)
	if err != nil {
		t.Fatal(results)
	}
	if out, err := compatScript(t, "passing", results); err != nil || out != "codex 0.172.0" {
		t.Errorf("passing = %q, %v", out, err)
	}
	if out, err := compatScript(t, "failing", results); err != nil || out != "opencode 2.4.1" {
		t.Errorf("failing = %q, %v", out, err)
	}
	onlyOC, _ := compatScript(t, "legs", `[{"harness":"opencode","version":"2.4.1"}]`)
	okJobs := compatJobs(t, passJob("harness-opencode-linux"), passJob("harness-opencode-macos"))
	okRes, err := compatScript(t, "report", okJobs, onlyOC)
	if err != nil {
		t.Fatal(okRes)
	}
	if out, _ := compatScript(t, "passing", okRes); out != "opencode 2.4.1" {
		t.Errorf("opencode-only passing = %q", out)
	}
	if out, _ := compatScript(t, "failing", okRes); out != "" {
		t.Errorf("opencode-only failing = %q", out)
	}
	rec := filepath.Join(t.TempDir(), "rec.json")
	orig := "{\n  \"codex\": {\"tested\": \"0.171.0\", \"checked\": \"2026-11-20\"},\n  \"claude-code\": {\"tested\": \"2.1.329\", \"checked\": \"2026-11-20\"}\n}\n"
	if err := os.WriteFile(rec, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := compatScript(t, "bump-record", rec, "opencode", "2.4.1", "2026-11-23"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	b, _ := os.ReadFile(rec)
	want := strings.TrimSuffix(orig, "\n}\n") + ",\n  \"opencode\": {\"tested\": \"2.4.1\", \"checked\": \"2026-11-23\"}\n}\n"
	if string(b) != want {
		t.Errorf("record after the opencode bump:\n%s\nwant\n%s", b, want)
	}

	fake, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeIssueGH), 0o755); err != nil {
		t.Fatal(err)
	}
	jf := compatJobs(t,
		compatJob("harness-codex-linux", "failure", "Run the leg"),
		compatJob("compare-opencode-macos", "failure", "Harness probes"),
		compatJob("harness-opencode-macos", "failure", "Run the leg"))
	cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), "file-issue", "Owner/Repo", "opencode", "2.4.1", "https://github.com/Owner/Repo/actions/runs/7", jf)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_DIR="+fake)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	created, _ := os.ReadFile(filepath.Join(fake, "created"))
	c := string(created)
	for _, w := range []string{"harness-compat: opencode 2.4.1 did not pass", "version 2.4.1", "compare-opencode-macos: Harness probes", "harness-opencode-macos: Run the leg", "--label harness-compat"} {
		if !strings.Contains(c, w) {
			t.Errorf("issue lacks %q:\n%s", w, c)
		}
	}
	if strings.Contains(c, "codex") {
		t.Errorf("an opencode issue lists a codex job:\n%s", c)
	}
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	for _, id := range []string{home, os.Getenv("USER"), host, "/Users/", "/home/"} {
		if id != "" && strings.Contains(c, id) {
			t.Errorf("issue text contains identifying %q:\n%s", id, c)
		}
	}
}

const fakeOpenCodeServe = `import json, os, socket, sys, time
port = int(os.environ["COMPAT_OPENCODE_PORT"])
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", port))
s.listen(5)
d = os.path.join(os.environ["XDG_STATE_HOME"], "opencode")
os.makedirs(d, exist_ok=True)
f = os.path.join(d, "service.json")
host = os.environ.get("FAKE_HOST", "127.0.0.1")
doc = {"version": "2.4.1", "url": "http://%s:%d" % (host, port), "pid": os.getpid() + int(os.environ.get("FAKE_PID_DELTA", "0")), "password": os.environ.get("FAKE_PASSWORD", "sekret")}
open(f, "w").write(json.dumps(doc))
os.chmod(f, int(os.environ.get("FAKE_MODE", "600"), 8))
time.sleep(60)
`

func TestHarnessCompatCompareOpenCodeFailsClosed(t *testing.T) {
	for _, tool := range []string{"jq", "python3", "lsof"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	freePort := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	cases := []struct {
		name, version string
		env           []string
		busy          bool
		reason        string
	}{
		{"banner before version", "echo banner; echo 'opencode v2.4.1'", nil, false, "version"},
		{"unparseable version", "echo 'opencode dev'", nil, false, "version"},
		{"service.json mode 0644", "echo 'opencode v2.4.1'", []string{"FAKE_MODE=644"}, false, "mode"},
		{"non-loopback url", "echo 'opencode v2.4.1'", []string{"FAKE_HOST=10.1.2.3"}, false, "loopback"},
		{"pid is not the listener", "echo 'opencode v2.4.1'", []string{"FAKE_PID_DELTA=7"}, false, "pid"},
		{"empty password", "echo 'opencode v2.4.1'", []string{"FAKE_PASSWORD="}, false, "password"},
		{"port held by another process", "echo 'opencode v2.4.1'", nil, true, "port"},
		{"every service.json check passes, then the session call has no server", "echo 'opencode v2.4.1'", nil, false, "session"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin, home := t.TempDir(), t.TempDir()
			serve := filepath.Join(bin, "serve.py")
			if err := os.WriteFile(serve, []byte(fakeOpenCodeServe), 0o644); err != nil {
				t.Fatal(err)
			}
			fake := "#!/bin/sh\ncase \"$1\" in --version) " + c.version + " ;; serve) exec python3 -I \"$FAKE_SERVE\" ;; esac\n"
			if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			port := freePort()
			if c.busy {
				l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
			}
			cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), "compare-opencode", home)
			cmd.Env = append(os.Environ(), append([]string{
				"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
				"FAKE_SERVE=" + serve, fmt.Sprintf("COMPAT_OPENCODE_PORT=%d", port), "COMPAT_WAIT_TRIES=6",
			}, c.env...)...)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("passed, want failure mentioning %q\n%s", c.reason, out)
			}
			if !strings.Contains(strings.ToLower(string(out)), c.reason) {
				t.Errorf("failure lacks the reason %q:\n%s", c.reason, out)
			}
			if strings.Contains(string(out), "sekret") {
				t.Errorf("the password reached the output:\n%s", out)
			}
		})
	}
}

func TestHarnessCompatOpenCodeServiceChecksTheListenerPid(t *testing.T) {
	for _, tool := range []string{"jq", "python3", "lsof"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	script := filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	f := filepath.Join(t.TempDir(), "service.json")
	write := func(pid int, host string) {
		doc := fmt.Sprintf(`{"version":"2.4.1","url":"http://%s:%d","pid":%d,"password":"sekret"}`, host, port, pid)
		if err := os.WriteFile(f, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		pid  int
		host string
		ok   bool
	}{
		{"pid is the listener", os.Getpid(), "127.0.0.1", true},
		{"pid is another process", os.Getpid() + 1, "127.0.0.1", false},
		{"url is not loopback", os.Getpid(), "10.1.2.3", false},
	}
	for _, c := range cases {
		write(c.pid, c.host)
		out, err := exec.Command(script, "opencode-service", f).CombinedOutput()
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v\n%s", c.name, err, c.ok, out)
		}
		if strings.Contains(string(out), "sekret") {
			t.Errorf("%s: the password reached the output:\n%s", c.name, out)
		}
		if c.ok && strings.TrimSpace(string(out)) != fmt.Sprintf("http://127.0.0.1:%d", port) {
			t.Errorf("%s: printed %q", c.name, out)
		}
	}
}

func TestHarnessCompatOpenCodeLegShape(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if n := strings.Count(src, "\n\topencode-linux | opencode-macos)\n\t\topencode_roundtrip\n"); n != 1 {
		t.Errorf("both opencode legs must run the shared opencode_roundtrip once; arms = %d", n)
	}
	if !strings.Contains(src, `jq '.password = "***"'`) || !strings.Contains(src, "--config -") {
		t.Error("the staged service.json must have its password redacted and the Basic header must go through curl --config -")
	}
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "curl") && strings.Contains(line, "OPENROUTER_API_KEY") && !strings.Contains(line, "key-usage") && !strings.Contains(line, "@$hdr") {
			t.Errorf("line %d puts the key near a curl command line: %s", i+1, strings.TrimSpace(line))
		}
	}
	wf := readCompatWorkflow(t)
	for _, want := range []string{"compare-opencode", "install \"$name\" --yes", "opencode) pkg=@opencode/cli", "OPENCODE_MODEL: ${{ vars.HARNESS_COMPAT_OPENCODE_MODEL }}"} {
		if !strings.Contains(wf, want) {
			t.Errorf("workflow lacks %q", want)
		}
	}
}

func TestHarnessCompatCompareOpenCodeChecksTheCandidateVersion(t *testing.T) {
	for _, tool := range []string{"jq", "python3", "lsof"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	bin, home := t.TempDir(), t.TempDir()
	fake := "#!/bin/sh\ncase \"$1\" in --version) echo 'opencode v2.4.0' ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), "compare-opencode", home, "2.4.1")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "candidate") {
		t.Fatalf("a different opencode on PATH must fail with the candidate named: %v\n%s", err, out)
	}
}

func TestHarnessCompatOpenCodeLegKeepsAnExistingConfig(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, ".config", "opencode", "opencode.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"), "leg", "opencode-linux", home)
	cmd.Env = append(os.Environ(), "OPENROUTER_API_KEY=k", "OPENCODE_MODEL=m", "CROSSTALK_BIN=/usr/bin/true", "LIVE_LOGS="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "scratch home") {
		t.Fatalf("an existing opencode.json must stop the leg: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(cfg); string(b) != "sentinel" {
		t.Errorf("the config was overwritten: %q", b)
	}
}

func TestHarnessCompatStageLogsScrubsTheOpenCodeServicePassword(t *testing.T) {
	raw, dest := t.TempDir(), filepath.Join(t.TempDir(), "out")
	pw := "svc-pass-xyz"
	basic := "b3BlbmNvZGU6c3ZjLXBhc3MteHl6"
	if err := os.WriteFile(filepath.Join(raw, "opencode-service.log"), []byte("start password="+pw+" Authorization: Basic "+basic+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"), "stage-logs", raw, dest)
	cmd.Env = append(os.Environ(), "OPENROUTER_API_KEY=k-123", "OPENCODE_SERVICE_PASSWORD="+pw)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "opencode-service.log"))
	if strings.Contains(string(got), pw) || strings.Contains(string(got), basic) {
		t.Errorf("staged log keeps the service credential: %s", got)
	}
}

func TestHarnessCompatOpenCodeServiceMustBeTheLaunchedPid(t *testing.T) {
	for _, tool := range []string{"jq", "lsof"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	f := filepath.Join(t.TempDir(), "service.json")
	doc := fmt.Sprintf(`{"url":"http://127.0.0.1:%d","pid":%d,"password":"x"}`, l.Addr().(*net.TCPAddr).Port, os.Getpid())
	if err := os.WriteFile(f, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh")
	if out, err := exec.Command(script, "opencode-service", f, fmt.Sprint(os.Getpid())).CombinedOutput(); err != nil {
		t.Errorf("launched pid matches: %v\n%s", err, out)
	}
	if out, err := exec.Command(script, "opencode-service", f, fmt.Sprint(os.Getpid()+1)).CombinedOutput(); err == nil {
		t.Errorf("a service this leg did not launch was accepted:\n%s", out)
	}
}

func TestHarnessCompatPiPkgAndCheckUpdates(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	root := releaseRepoRoot(t)
	bin := t.TempDir()
	fake := "#!/bin/sh\necho \"$*\" >> \"$FAKE_LOG\"\ncase \"$2\" in @earendil-works/pi-coding-agent) echo 1.6.0 ;; @opencode/cli) echo 2.4.1 ;; @openai/codex) echo 0.171.0 ;; *) echo 2.1.330 ;; esac\n"
	if err := os.WriteFile(filepath.Join(bin, "npm"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := filepath.Join(bin, "record.json")
	if err := os.WriteFile(rec, []byte(`{"codex": {"tested": "0.171.0"}, "claude-code": {"tested": "2.1.330"}, "opencode": {"tested": "2.4.1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	logf := filepath.Join(bin, "npm.log")
	run := func(args ...string) (string, error) {
		cmd := exec.Command(filepath.Join(root, "scripts", "harness-compat.sh"), append([]string{"check-updates", rec}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_LOG="+logf)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run("all", "", "", ""); err != nil || out != `[{"harness":"pi","version":"1.6.0"}]` {
		t.Fatalf("all must include an unrecorded pi: %q, %v", out, err)
	}
	if b, _ := os.ReadFile(logf); !strings.Contains(string(b), "view @earendil-works/pi-coding-agent version") {
		t.Errorf("npm was asked for: %s", b)
	}
	if out, err := run("cursor", "", "", ""); err == nil || !strings.Contains(out, "unknown harness") {
		t.Errorf("an unknown harness must fail closed: %q, %v", out, err)
	}
}

func TestHarnessCompatPiRecordEntry(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), ".github", "harness-compat.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]struct{ Tested, Checked string }
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	e := rec["pi"]
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(e.Tested) || !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(e.Checked) {
		t.Fatalf("pi record = %+v", e)
	}
}

func TestHarnessCompatPiLegsReportBumpAndIssue(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := compatScript(t, args...)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	only := run("legs", `[{"harness":"pi","version":"1.6.0"}]`, "0.167.0")
	if want := `[{"leg":"pi-linux","harness":"pi","version":"1.6.0","runner":"ubuntu-latest"},{"leg":"pi-macos","harness":"pi","version":"1.6.0","runner":"macos-latest"}]`; only != want {
		t.Fatalf("pi legs = %s, want %s", only, want)
	}
	okRes := run("report", compatJobs(t, passJob("harness-pi-linux"), passJob("harness-pi-macos")), only)
	if got := run("passing", okRes); got != "pi 1.6.0" {
		t.Errorf("passing = %q", got)
	}
	for name, jobs := range map[string]string{
		"macOS leg failed":   compatJobs(t, passJob("harness-pi-linux"), compatJob("harness-pi-macos", "failure", "Run the leg")),
		"macOS leg missing":  compatJobs(t, passJob("harness-pi-linux")),
		"leg skipped":        compatJobs(t, passJob("harness-pi-linux"), compatJob("harness-pi-macos", "skipped")),
		"leg step never ran": compatJobs(t, passJob("harness-pi-linux"), compatJob("harness-pi-macos", "success")),
	} {
		res := run("report", jobs, only)
		if run("passing", res) != "" || run("failing", res) != "pi 1.6.0" {
			t.Errorf("%s: must hold the bump and file an issue: %s", name, res)
		}
	}
	rec := filepath.Join(t.TempDir(), "rec.json")
	if err := os.WriteFile(rec, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("bump-record", rec, "pi", "1.6.0", "2026-11-25")
	if b, _ := os.ReadFile(rec); !strings.Contains(string(b), `"pi": {"tested": "1.6.0", "checked": "2026-11-25"}`) {
		t.Errorf("record = %s", b)
	}
	fake, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeIssueGH), 0o755); err != nil {
		t.Fatal(err)
	}
	jf := compatJobs(t, compatJob("compare-pi-macos", "failure", "Harness probes"), compatJob("harness-codex-linux", "failure", "Run the leg"))
	cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), "file-issue", "Owner/Repo", "pi", "1.6.0", "https://github.com/Owner/Repo/actions/runs/7", jf)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_DIR="+fake)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	c, _ := os.ReadFile(filepath.Join(fake, "created"))
	if !strings.Contains(string(c), "harness-compat: pi 1.6.0 did not pass") || !strings.Contains(string(c), "compare-pi-macos: Harness probes") || strings.Contains(string(c), "codex") {
		t.Errorf("issue:\n%s", c)
	}
}

func TestHarnessCompatPiWorkflow(t *testing.T) {
	jobs := workflowJobs(readCompatWorkflow(t))
	w := readCompatWorkflow(t)
	for job, wants := range map[string][]string{
		"compare": {"pi) pkg=@earendil-works/pi-coding-agent", "pi) name=pi", `compare-pi "$SCRATCH_HOME" "$VERSION"`, "node-version: 24"},
		"harness": {
			"PI_COMPAT_MODEL: ${{ vars.HARNESS_COMPAT_PI_MODEL }}",
			`pi) model="$PI_COMPAT_MODEL"; var=HARNESS_COMPAT_PI_MODEL ;;`,
			`[ -n "$model" ] || { echo "::error::$var is not set"; exit 1; }`,
			`npm install -g "@earendil-works/pi-coding-agent@$VERSION"; [ "$(pi --version)" = "$VERSION" ]`,
			"node-version: 24",
		},
	} {
		for _, want := range wants {
			if !strings.Contains(jobs[job], want) {
				t.Errorf("job %s lacks %q", job, want)
			}
		}
	}
	if !strings.Contains(w, "(all, codex, claude-code, opencode or pi)") {
		t.Error("the dispatch input must name pi")
	}
	if strings.Contains(jobs["compare"], "secrets.") || strings.Contains(jobs["compare"], "OPENROUTER") {
		t.Error("the no-token legs hold no secret")
	}
	floor := map[string]int{"checkout": 6, "cache": 5, "upload-artifact": 5, "download-artifact": 6, "setup-node": 7, "setup-go": 7}
	for _, m := range regexp.MustCompile(`uses: actions/([a-z-]+)@v(\d+)`).FindAllStringSubmatch(w, -1) {
		if n, _ := strconv.Atoi(m[2]); floor[m[1]] != 0 && n < floor[m[1]] {
			t.Errorf("actions/%s@v%d is below v%d", m[1], n, floor[m[1]])
		}
	}
}

func TestHarnessCompatPiLegShape(t *testing.T) {
	root := releaseRepoRoot(t)
	read := func(n string) string {
		b, err := os.ReadFile(filepath.Join(root, "scripts", n))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	sc, live := read("harness-compat.sh"), read("harness-compat-live.sh")
	i := strings.Index(sc, "\ncompare-pi)")
	if i < 0 {
		t.Fatal("no compare-pi")
	}
	arm := sc[i:]
	arm = arm[:strings.Index(arm, "\n\t;;\n")]
	for _, want := range []string{
		`export HOME="$home" PI_CODING_AGENT_DIR="$home/.pi/agent" PI_OFFLINE=1 PI_TELEMETRY=0`,
		"unset OPENROUTER_API_KEY",
		"script -qfec", "script -q /dev/null",
		`.state == "mailboxed"`, `"$0" pi-wake`,
	} {
		if !strings.Contains(arm, want) {
			t.Errorf("compare-pi lacks %q", want)
		}
	}
	if want := "wake='" + pi.WakeText + "'"; !strings.Contains(sc, want) {
		t.Errorf("pi-wake must pin the shim's fixed wake: %s", want)
	}
	if n := strings.Count(live, "\n\tpi-linux | pi-macos)\n\t\tpi_roundtrip\n"); n != 1 {
		t.Errorf("both pi legs must run pi_roundtrip once; arms = %d", n)
	}
	for _, want := range []string{`pi --model "$PI_COMPAT_MODEL"`, `export PI_CODING_AGENT_DIR="$home/.pi/agent" PI_TELEMETRY=0`, "assert-pi-no-turn", `"$logs/pi-sessions"`} {
		if !strings.Contains(live, want) {
			t.Errorf("live leg lacks %q", want)
		}
	}
	printMode := regexp.MustCompile(`(^|[;&|( ])pi +([^|;]* )?(-p|--print)( |$)`)
	for _, src := range []string{sc, live} {
		for n, line := range strings.Split(src, "\n") {
			if printMode.MatchString(line) && !strings.Contains(line, "</dev/null") {
				t.Errorf("line %d: pi -p without </dev/null hangs on piped stdin: %s", n+1, strings.TrimSpace(line))
			}
		}
	}
}

const piSession = "01a21575-4ddd-70c0-8a54-282e681e5104"

func piSessionDir(t *testing.T, lines ...map[string]any) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "--work--")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(journalLine(t, l) + "\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-11-26T00-00-00-000Z_"+piSession+".jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(dir)
}

func piCustom(content string) map[string]any {
	return map[string]any{"type": "custom_message", "customType": "crosstalk", "content": content}
}

func piMessage(role, stop string) map[string]any {
	return map[string]any{"type": "message", "message": map[string]any{"role": role, "stopReason": stop, "content": []any{}}}
}

func TestHarnessCompatPiWakeCheck(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	sys, wake := piMessage("system", ""), piCustom(pi.WakeText)
	cases := []struct {
		name  string
		lines []map[string]any
		ok    bool
		out   string
	}{
		{"admitted, run started, provider failed", []map[string]any{sys, wake, piMessage("assistant", "error")}, true, "admitted=yes run_started=yes provider_stop=error"},
		{"nothing in the session", []map[string]any{sys}, false, "wake"},
		{"admitted but no run", []map[string]any{sys, wake}, false, "no run started"},
		{"another text", []map[string]any{sys, piCustom("hello"), piMessage("assistant", "error")}, false, "wake"},
		{"a second custom message", []map[string]any{sys, wake, piCustom("more"), piMessage("assistant", "error")}, false, "wake"},
		{"a user message", []map[string]any{sys, wake, piMessage("user", ""), piMessage("assistant", "error")}, false, "wake"},
		{"the peer body", []map[string]any{sys, wake, piCustom("compat-body"), piMessage("assistant", "error")}, false, "body"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := compatScript(t, "pi-wake", piSessionDir(t, c.lines...), piSession, "compat-body")
			if (err == nil) != c.ok || !strings.Contains(out, c.out) {
				t.Errorf("err = %v, want ok=%v with %q\n%s", err, c.ok, c.out, out)
			}
		})
	}
	if out, err := compatScript(t, "pi-wake", t.TempDir(), piSession, "compat-body"); err == nil {
		t.Errorf("no session file passed: %s", out)
	}
}

func TestHarnessCompatPiNoTurnCheck(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "journal"), 0o755); err != nil {
		t.Fatal(err)
	}
	journal := func(st string) {
		j := journalLine(t, map[string]any{"type": "dedupe", "message_id": "m1", "message": map[string]any{"parts": []map[string]any{{"type": "text", "text": "compat-NT"}}}}) + "\n" +
			journalLine(t, map[string]any{"type": "outcome", "message_id": "m1", "state": st}) + "\n"
		if err := os.WriteFile(filepath.Join(state, "journal", "recv.jsonl"), []byte(j), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before, shown := piMessage("assistant", "stop"), piCustom("preamble compat-NT")
	cases := []struct {
		name, state string
		lines       []map[string]any
		ok          bool
	}{
		{"shown, queued, no turn", "queued", []map[string]any{before, shown}, true},
		{"a turn followed", "queued", []map[string]any{before, shown, piMessage("assistant", "stop")}, false},
		{"never shown", "queued", []map[string]any{before}, false},
		{"only the fixed wake", "queued", []map[string]any{before, piCustom(pi.WakeText)}, false},
		{"not queued", "unknown", []map[string]any{before, shown}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			journal(c.state)
			out, err := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"), "assert-pi-no-turn", state, "recv", piSessionDir(t, c.lines...), piSession, "compat-NT").CombinedOutput()
			if (err == nil) != c.ok {
				t.Errorf("err = %v, want ok=%v\n%s", err, c.ok, out)
			}
		})
	}
}

// fakePi stands in for Pi under the pty: it registers a "shim" child in the
// scratch state and obeys /new and /quit. FAKE_BREAK names the one behaviour
// it gets wrong.
const fakePi = `#!/usr/bin/env python3
import json, os, subprocess, sys
brk = os.environ.get("FAKE_BREAK", "")
if sys.argv[1:] == ["--version"]:
    print({"banner": "hello\n1.6.0", "candidate": "1.5.0"}.get(brk, "1.6.0"))
    sys.exit()
home = os.environ["HOME"]
if os.environ.get("PI_OFFLINE") != "1" or os.environ.get("PI_CODING_AGENT_DIR") != home + "/.pi/agent" or "OPENROUTER_API_KEY" in os.environ:
    sys.exit("fake pi: not a scratch offline environment")
agents = home + "/.local/state/crosstalk/agents"
os.makedirs(agents, exist_ok=True)
def shim():
    return subprocess.Popen(["sleep", "60"], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
def describe(n, p):
    d = {"agent_id": "a%d" % n, "harness": "pi", "pid": p.pid, "harness_pid": os.getpid() + (brk == "parent"), "session_ref": "s%d@0" % n}
    open(agents + "/a.json", "w").write(json.dumps(d))
child = None
if brk != "register":
    child = shim()
    describe(1, child)
for line in sys.stdin:
    cmd = line.strip()
    if cmd.endswith("/new") and brk != "new":
        if brk != "old-shim":
            child.kill()
            child.wait()
        child = shim()
        describe(2, child)
    elif cmd.endswith("/quit"):
        if child and brk != "quit":
            child.kill()
            os.remove(agents + "/a.json")
        break
`

const fakeCrosstalk = `#!/usr/bin/env python3
import glob, json, os, sys
brk = os.environ.get("FAKE_BREAK", "")
home = os.environ["HOME"]
state = home + "/.local/state/crosstalk"
ds = [json.load(open(f)) for f in glob.glob(state + "/agents/*.json")]
if sys.argv[1] == "mcp":
    for line in sys.stdin:
        if json.loads(line).get("id") == 2:
            rows = [{"agent_id": d["agent_id"], "harness": "pi", "reachable": brk != "reachable"} for d in ds]
            print(json.dumps({"id": 2, "result": {"structuredContent": {"agents": rows}}}), flush=True)
    sys.exit()
to, body = sys.argv[2], sys.argv[3]
st = "accepted" if brk == "state" else "mailboxed"
os.makedirs(state + "/journal", exist_ok=True)
with open(state + "/journal/" + to + ".jsonl", "w") as j:
    if brk != "journal":
        j.write(json.dumps({"type": "dedupe", "message_id": "m1", "message": {"parts": [{"type": "text", "text": body}]}}) + "\n")
    j.write(json.dumps({"type": "outcome", "message_id": "m1", "state": st}) + "\n")
d = os.environ["PI_CODING_AGENT_DIR"] + "/sessions/w"
os.makedirs(d, exist_ok=True)
ref = ds[0]["session_ref"].split("@")[0]
with open(d + "/t_" + ref + ".jsonl", "w") as s:
    s.write(json.dumps({"type": "custom_message", "customType": "crosstalk", "content": body if brk == "leak" else "A CrossTalk message is waiting: call receive_message"}) + "\n")
    s.write(json.dumps({"type": "message", "message": {"role": "assistant", "stopReason": "error"}}) + "\n")
print(json.dumps({"message_id": "m1", "state": st}))
sys.exit(1)
`

func TestHarnessCompatComparePiFailsClosed(t *testing.T) {
	for _, tool := range []string{"jq", "python3", "script"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not on PATH")
		}
	}
	for _, c := range []struct{ brk, reason string }{
		{"", ""},
		{"banner", "no longer parses"},
		{"candidate", "candidate"},
		{"no-extension", "no extension"},
		{"register", "did not register"},
		{"parent", "parent"},
		{"reachable", "reachable"},
		{"new", "/new"},
		{"old-shim", "old shim"},
		{"state", "mailboxed"},
		{"journal", "journal"},
		{"leak", "body"},
		{"quit", "after pi quit"},
	} {
		t.Run("break="+c.brk, func(t *testing.T) {
			t.Parallel()
			bin, home := t.TempDir(), t.TempDir()
			for name, src := range map[string]string{"pi": fakePi, "crosstalk": fakeCrosstalk} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(src), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if c.brk != "no-extension" {
				ext := filepath.Join(home, ".pi", "agent", "extensions", "crosstalk")
				if err := os.MkdirAll(ext, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(ext, "index.ts"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat.sh"), "compare-pi", home, "1.6.0")
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"CROSSTALK_BIN="+filepath.Join(bin, "crosstalk"), "FAKE_BREAK="+c.brk, "COMPAT_WAIT_TRIES=8", "OPENROUTER_API_KEY=k-should-not-reach-pi")
			cmd.WaitDelay = 10 * time.Second
			out, err := cmd.CombinedOutput()
			if c.brk == "" {
				if err != nil || !strings.Contains(string(out), "pi registration, /new, mailbox wake and quit ok") {
					t.Fatalf("the good fake must pass: %v\n%s", err, out)
				}
				return
			}
			if err == nil || !strings.Contains(string(out), c.reason) {
				t.Errorf("err = %v, want a failure naming %q\n%s", err, c.reason, out)
			}
		})
	}
}

func TestHarnessCompatPiLegKeepsAnExistingAgentDir(t *testing.T) {
	home := t.TempDir()
	f := filepath.Join(home, ".pi", "agent", "settings.json")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("sentinel"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(releaseRepoRoot(t), "scripts", "harness-compat-live.sh"), "leg", "pi-linux", home)
	cmd.Env = append(os.Environ(), "OPENROUTER_API_KEY=k", "PI_COMPAT_MODEL=m", "CROSSTALK_BIN=/usr/bin/true", "LIVE_LOGS="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "scratch home") {
		t.Fatalf("an existing ~/.pi must stop the leg: %v\n%s", err, out)
	}
}
