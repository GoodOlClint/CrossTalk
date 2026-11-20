package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness/codex"
)

const userHooks = `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"sh /x/check","timeout":3}]}],"Stop":[{"hooks":[{"type":"command","command":"sh /x/stop"}]}]},"extra":{"keep":true}}`

func queueWakeOS(t *testing.T, goos string) {
	old := codex.QueueWakeOS
	codex.QueueWakeOS = goos
	t.Cleanup(func() { codex.QueueWakeOS = old })
}

func hooksJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(installRead(t, p)), &v); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return v
}

func TestInstallQueueWakeFlag_OnAndUninstall_ExpectOnlyOwnEntriesChange(t *testing.T) {
	queueWakeOS(t, "darwin")
	home := installSandbox(t, "claude", "codex")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	hp := filepath.Join(home, ".codex", "hooks.json")
	installWrite(t, hp, userHooks)
	bin := filepath.Join(home, "bin")

	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	if codex.ClaimsEnabled(state) || installRead(t, hp) != userHooks {
		t.Fatal("install without the flag changed the setting or hooks.json")
	}

	code, out := installRunCmd(t, "install", "--yes", "--codex-no-daemon-queue", "--bin-dir", bin)
	if code != 0 {
		t.Fatalf("install with flag: %d\n%s", code, out)
	}
	if !codex.ClaimsEnabled(state) {
		t.Fatal("the flag did not turn the setting on")
	}
	if !strings.Contains(out, "experimental") {
		t.Errorf("output does not say experimental:\n%s", out)
	}
	h := hooksJSON(t, hp)["hooks"].(map[string]any)
	if g := h["SessionStart"].([]any); len(g) != 2 || !strings.Contains(installRead(t, hp), "hook codex session-start") {
		t.Fatalf("SessionStart groups %v", g)
	}
	if code, out := installRunCmd(t, "install", "--yes", "--codex-no-daemon-queue", "--bin-dir", bin); code != 0 || strings.Count(installRead(t, hp), "hook codex session-start") != 1 {
		t.Fatalf("second install: %d, hook count %d\n%s", code, strings.Count(installRead(t, hp), "hook codex session-start"), out)
	}

	if code, out := installRunCmd(t, "uninstall", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("uninstall: %d\n%s", code, out)
	}
	var want map[string]any
	json.Unmarshal([]byte(userHooks), &want)
	if got := hooksJSON(t, hp); !reflect.DeepEqual(got, want) {
		t.Errorf("hooks.json after uninstall = %v, want the user's original %v", got, want)
	}
	if codex.ClaimsEnabled(state) {
		t.Error("uninstall left the setting on")
	}
}

func TestInstallQueueWakeFlag_NoHooksFile_ExpectFileCreatedThenRemoved(t *testing.T) {
	queueWakeOS(t, "darwin")
	home := installSandbox(t, "codex")
	hp := filepath.Join(home, ".codex", "hooks.json")
	bin := filepath.Join(home, "bin")
	if code, out := installRunCmd(t, "install", "--yes", "--codex-no-daemon-queue", "--bin-dir", bin); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	if _, err := os.Stat(hp); err != nil {
		t.Fatalf("hooks.json not written: %v", err)
	}
	if code, out := installRunCmd(t, "uninstall", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("uninstall: %d\n%s", code, out)
	}
	if _, err := os.Stat(hp); !os.IsNotExist(err) {
		t.Errorf("hooks.json left behind: %v", err)
	}
}

func TestInstallQueueWakeFlag_ClaudeOnly_ExpectUsageError(t *testing.T) {
	installSandbox(t, "claude", "codex")
	if code, _ := installRunCmd(t, "install", "claude", "--yes", "--codex-no-daemon-queue"); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if code, _ := installRunCmd(t, "uninstall", "--yes", "--codex-no-daemon-queue"); code != 2 {
		t.Errorf("uninstall accepted the flag: exit %d, want 2", code)
	}
}

func TestInstall_StaleLegacySwitchFile_ExpectRemovedWithNoticeAndNeverEnables(t *testing.T) {
	home := installSandbox(t, "codex")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	old := filepath.Join(state, codex.LegacySettingFile)
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", filepath.Join(home, "bin")); code != 0 {
		t.Fatalf("first install: %d\n%s", code, out)
	}
	installWrite(t, old, "")
	if codex.ClaimsEnabled(state) {
		t.Fatal("the legacy file turned the setting on")
	}
	_, chk := installRunCmd(t, "install", "--check", "--bin-dir", filepath.Join(home, "bin"))
	if !strings.Contains(chk, "K10 codex WARN") || !strings.Contains(chk, codex.LegacySettingFile) {
		t.Errorf("--check does not report the stale file:\n%s", chk)
	}
	code, out := installRunCmd(t, "install", "--yes", "--bin-dir", filepath.Join(home, "bin"))
	if code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("stale file still there: %v", err)
	}
	if !strings.Contains(out, "--codex-no-daemon-queue") || codex.ClaimsEnabled(state) {
		t.Errorf("no notice naming the flag, or setting on:\n%s", out)
	}
}

func TestInstallCheck_K10_OffByDefault(t *testing.T) {
	home := installSandbox(t, "codex")
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", filepath.Join(home, "bin")); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	_, out := installRunCmd(t, "install", "--check", "--bin-dir", filepath.Join(home, "bin"))
	if !strings.Contains(out, "K10 codex PASS experimental --no-daemon queue wake is off") {
		t.Errorf("K10 line missing:\n%s", out)
	}
}

func TestInstallQueueWakeFlag_FailedHookWrite_ExpectSettingStaysOff(t *testing.T) {
	queueWakeOS(t, "darwin")
	for name, content := range map[string]string{"malformed": "{not json", "null": "null", "array": "[]"} {
		t.Run(name, func(t *testing.T) {
			home := installSandbox(t, "codex")
			state := filepath.Join(home, ".local", "state", "crosstalk")
			installWrite(t, filepath.Join(home, ".codex", "hooks.json"), content)
			if code, out := installRunCmd(t, "install", "--yes", "--codex-no-daemon-queue", "--bin-dir", filepath.Join(home, "bin")); code == 0 {
				t.Fatalf("install succeeded on a bad hooks.json:\n%s", out)
			}
			if codex.ClaimsEnabled(state) {
				t.Error("a failed opt-in left the setting on")
			}
			if got := installRead(t, filepath.Join(home, ".codex", "hooks.json")); got != content {
				t.Errorf("hooks.json rewritten: %q", got)
			}
		})
	}
}

func TestInstallQueueWakeFlag_NotMacOS_ExpectRefusedSettingOff(t *testing.T) {
	queueWakeOS(t, "linux")
	home := installSandbox(t, "codex")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	if code, _ := installRunCmd(t, "install", "--yes", "--codex-no-daemon-queue", "--bin-dir", filepath.Join(home, "bin")); code == 0 {
		t.Fatal("install accepted the flag off macOS")
	}
	if codex.ClaimsEnabled(state) {
		t.Error("setting on")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Errorf("hooks.json written: %v", err)
	}
}

func TestUninstall_UserCompoundHookEndingInOurCommand_ExpectKept(t *testing.T) {
	home := installSandbox(t, "codex")
	hp := filepath.Join(home, ".codex", "hooks.json")
	user := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"setup-env && '/opt/x/crosstalk' hook codex session-start"}]}]}}`
	installWrite(t, hp, user)
	bin := filepath.Join(home, "bin")
	for _, mode := range []string{"install", "uninstall"} {
		if code, out := installRunCmd(t, mode, "--yes", "--bin-dir", bin); code != 0 {
			t.Fatalf("%s: %d\n%s", mode, code, out)
		}
		if got := installRead(t, hp); got != user {
			t.Fatalf("after %s hooks.json = %q, want the user's file untouched", mode, got)
		}
	}
}

func TestInstallQueueWakeFlag_ApostropheInBinDir_ExpectHookInvokesBinaryAsOneArgument(t *testing.T) {
	queueWakeOS(t, "darwin")
	home := installSandbox(t, "codex")
	bin := filepath.Join(home, "O'Brien", "bin")
	if code, out := installRunCmd(t, "install", "--yes", "--codex-no-daemon-queue", "--bin-dir", bin); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	h := hooksJSON(t, filepath.Join(home, ".codex", "hooks.json"))["hooks"].(map[string]any)["SessionStart"].([]any)
	cmd := h[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"].(string)
	out, err := exec.Command("/bin/sh", "-c", strings.TrimSuffix(cmd, " hook codex session-start")+" --version-probe-not-run; true").CombinedOutput()
	if err != nil || strings.Contains(string(out), "syntax error") || strings.Contains(string(out), "unexpected EOF") {
		t.Fatalf("shell cannot parse %q: %v %s", cmd, err, out)
	}
	if code, out := installRunCmd(t, "uninstall", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("uninstall: %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "hooks.json")); !os.IsNotExist(err) {
		t.Errorf("the apostrophe-path hook was not removed: %v", err)
	}
}
