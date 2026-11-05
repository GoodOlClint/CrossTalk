package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func installWrite(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func installRead(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

// installUserFiles seeds a temp HOME with settings, Codex config and state.
func installUserFiles(t *testing.T, home string) (settings, config, state string) {
	settings = filepath.Join(home, ".claude", "settings.json")
	config = filepath.Join(home, ".codex", "config.toml")
	state = filepath.Join(home, ".local", "state", "crosstalk")
	installWrite(t, settings, "{\n  \"model\": \"x\",\n  \"permissions\": {\n    \"deny\": [\n      \"Bash(rm:*)\"\n    ]\n  }\n}\n")
	installWrite(t, config, "model = \"x\"\n")
	installWrite(t, filepath.Join(state, "journal", "a.jsonl"), "{}\n")
	installWrite(t, filepath.Join(state, "agents", "0123456789abcdef.lock"), "")
	return
}

func TestUninstallReverses(t *testing.T) {
	home := installSandbox(t, "claude", "codex")
	settings, config, state := installUserFiles(t, home)
	pre := map[string]string{settings: installRead(t, settings), config: installRead(t, config)}
	preState := installSnapshot(t, state)
	bin := filepath.Join(home, "bin")
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	if installRead(t, config) == pre[config] || installRead(t, settings) == pre[settings] {
		t.Fatal("install changed nothing")
	}
	code, out := installRunCmd(t, "uninstall", "--yes", "--bin-dir", bin)
	if code != 0 {
		t.Fatalf("uninstall: %d\n%s", code, out)
	}
	for p, want := range pre {
		if got := installRead(t, p); got != want {
			t.Errorf("%s:\ngot  %q\nwant %q", p, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(bin, "crosstalk")); !os.IsNotExist(err) {
		t.Errorf("binary still there: %v", err)
	}
	if fmt.Sprint(installSnapshot(t, state)) != fmt.Sprint(preState) {
		t.Error("state dir changed")
	}
	log := installRead(t, filepath.Join(home, "fake.log"))
	for _, c := range []string{"claude plugin uninstall crosstalk", "claude mcp remove --scope user crosstalk", "codex plugin remove crosstalk@crosstalk"} {
		if !strings.Contains(log, c+"\n") {
			t.Errorf("not run: %s", c)
		}
	}
}

func TestReinstallReappliesAll(t *testing.T) {
	home := installSandbox(t, "claude", "codex")
	bin := filepath.Join(home, "bin")
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	os.Remove(filepath.Join(home, "fake.log"))
	if code, out := installRunCmd(t, "reinstall", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("reinstall: %d\n%s", code, out)
	}
	log := installRead(t, filepath.Join(home, "fake.log"))
	for _, c := range []string{"claude plugin marketplace update crosstalk", "claude plugin update crosstalk@crosstalk", "claude mcp remove --scope user crosstalk", "claude mcp add --scope user crosstalk", "codex plugin add crosstalk@crosstalk", "codex mcp list"} {
		if !strings.Contains(log, c) {
			t.Errorf("reinstall did not run %s\n%s", c, log)
		}
	}
}

func TestUninstallPurge(t *testing.T) {
	home := installSandbox(t, "claude")
	_, _, state := installUserFiles(t, home)
	bin := filepath.Join(home, "bin")
	install := func() {
		if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
			t.Fatalf("install: %d\n%s", code, out)
		}
	}
	install()
	pre := installSnapshot(t, state)
	if code, out := installRunCmd(t, "uninstall", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("uninstall: %d\n%s", code, out)
	}
	if fmt.Sprint(installSnapshot(t, state)) != fmt.Sprint(pre) {
		t.Error("state changed without --purge")
	}

	install()
	f, err := os.OpenFile(filepath.Join(state, "agents", "0123456789abcdef.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	code, out := installRunCmd(t, "uninstall", "--yes", "--purge", "--bin-dir", bin)
	if code != 1 || !strings.Contains(out, "0123456789abcdef") {
		t.Errorf("purge with a held lock: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(state); err != nil {
		t.Errorf("state dir gone with a lock held: %v", err)
	}
	f.Close()

	install()
	if code, out := installRunCmd(t, "uninstall", "--yes", "--purge", "--bin-dir", bin); code != 0 {
		t.Fatalf("purge: %d\n%s", code, out)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("state dir still there: %v", err)
	}
}

func TestScopedUninstallKeepsSharedBinary(t *testing.T) {
	home := installSandbox(t, "claude", "codex")
	bin := filepath.Join(home, "bin")
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	if code, out := installRunCmd(t, "uninstall", "claude", "--yes", "--purge", "--bin-dir", bin); code != 0 {
		t.Fatalf("uninstall claude: %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(bin, "crosstalk")); err != nil {
		t.Fatalf("shared binary deleted: %v", err)
	}
	code, out := installRunCmd(t, "install", "codex", "--check", "--bin-dir", bin)
	if code != 0 || !strings.Contains(installCheckLine(out, "K6", "codex"), "PASS") {
		t.Errorf("codex after uninstall claude: exit %d\n%s", code, out)
	}
	if code, out := installRunCmd(t, "uninstall", "codex", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("uninstall codex: %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(bin, "crosstalk")); !os.IsNotExist(err) {
		t.Errorf("binary still there after the last uninstall: %v", err)
	}
}

func TestUninstallKeepsWhenOwnershipUncertain(t *testing.T) {
	home := installSandbox(t, "claude", "codex")
	_, config, state := installUserFiles(t, home)
	bin := filepath.Join(home, "bin")
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("install: %d\n%s", code, out)
	}
	path := os.Getenv("PATH")
	os.Remove(filepath.Join(path, "codex"))
	if code, out := installRunCmd(t, "uninstall", "--yes", "--purge", "--bin-dir", bin); code != 0 {
		t.Fatalf("uninstall without codex on PATH: %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(bin, "crosstalk")); err != nil {
		t.Errorf("binary deleted while codex config still runs it: %v", err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Errorf("state purged while codex is still set up: %v", err)
	}

	if code, out := installRunCmd(t, "install", "claude", "--yes", "--bin-dir", bin); code != 0 {
		t.Fatalf("reinstall claude: %d\n%s", code, out)
	}
	os.Chmod(config, 0)
	defer os.Chmod(config, 0o600)
	if code, out := installRunCmd(t, "uninstall", "--yes", "--purge", "--bin-dir", bin); code != 0 {
		t.Fatalf("uninstall with an unreadable codex config: %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(bin, "crosstalk")); err != nil {
		t.Errorf("binary deleted with an unreadable codex config: %v", err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Errorf("state purged with an unreadable codex config: %v", err)
	}
}
