package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installSandbox gives the test a temp HOME, CODEX_HOME and a PATH holding
// only fake harness CLIs, each of which runs TestInstallFakeCLI.
func installSandbox(t *testing.T, clis ...string) string {
	t.Helper()
	real := installBuild(t, ".")
	old := installExecutable
	installExecutable = func() (string, error) { return real, nil }
	t.Cleanup(func() { installExecutable = old })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("SHELL", "/bin/sh")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, c := range clis {
		script := fmt.Sprintf("#!/bin/sh\nINSTALL_FAKE_CLI=%s exec %q -test.run='^TestInstallFakeCLI$' -- \"$@\"\n", c, exe)
		if err := os.WriteFile(filepath.Join(bin, c), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return home
}

// TestInstallFakeCLI is the fake claude and codex CLI; it does nothing
// unless started by a script from installSandbox.
func TestInstallFakeCLI(t *testing.T) {
	name := os.Getenv("INSTALL_FAKE_CLI")
	if name == "" {
		t.Skip("fake CLI only")
	}
	var args []string
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	os.Exit(installFake(name, args))
}

func installSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		v := info.Mode().String()
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += fmt.Sprintf(" %x", sha256.Sum256(b))
		}
		snap[p] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func installRunCmd(t *testing.T, mode string, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := runInstall(mode, args, strings.NewReader(""), false, &out, &out)
	return code, out.String()
}

func TestInstallSelectsHarnesses(t *testing.T) {
	home := installSandbox(t, "claude")
	code, out := installRunCmd(t, "install", "--yes", "--bin-dir", filepath.Join(home, "bin"))
	if code != 0 {
		t.Fatalf("install --yes: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "== claude") || strings.Contains(out, "== codex") {
		t.Errorf("install acted on the wrong harnesses:\n%s", out)
	}
	if !strings.Contains(out, "codex: not found") {
		t.Errorf("install did not name codex as not found:\n%s", out)
	}
	if code, out := installRunCmd(t, "install", "codex", "--yes", "--bin-dir", filepath.Join(home, "bin")); code != 1 {
		t.Errorf("install codex with codex absent: exit %d, want 1\n%s", code, out)
	}
	if code, _ := installRunCmd(t, "install", "bogus", "--yes"); code != 2 {
		t.Errorf("install bogus: exit %d, want 2", code)
	}
}

func TestInstallRefusesWithoutTerminal(t *testing.T) {
	home := installSandbox(t, "claude", "codex")
	for p, c := range map[string]string{
		".claude/settings.json": `{"permissions": {"deny": ["mcp__plugin_crosstalk_crosstalk__ask"]}}`,
		".claude.json":          `{"projects": {}}`,
		".codex/config.toml":    "model = \"x\"\n",
		".profile":              "# profile\n",
		".zprofile":             "# zprofile\n",
	} {
		os.MkdirAll(filepath.Dir(filepath.Join(home, p)), 0o755)
		if err := os.WriteFile(filepath.Join(home, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := installSnapshot(t, home)
	for mode, flag := range map[string]string{"install": "--path", "reinstall": "--path", "uninstall": "--purge"} {
		code, out := installRunCmd(t, mode, flag, "--bin-dir", filepath.Join(home, "bin"))
		if code != 2 {
			t.Errorf("%s without terminal: exit %d, want 2\n%s", mode, code, out)
		}
	}
	after := installSnapshot(t, home)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("HOME changed:\nbefore %v\nafter  %v", before, after)
	}
}

// TestInstallHelperSleep is a long-running process for the binary tests; it
// does nothing unless INSTALL_HELPER_SLEEP is set.
func TestInstallHelperSleep(t *testing.T) {
	if os.Getenv("INSTALL_HELPER_SLEEP") == "" {
		t.Skip("helper process only")
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

func installCopyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInstallBinaryReplacesRunningFile(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "crosstalk")
	installCopyFile(t, exe, dst)
	cmd := exec.Command(dst, "-test.run=^TestInstallHelperSleep$")
	cmd.Env = append(os.Environ(), "INSTALL_HELPER_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer cmd.Process.Kill()
	time.Sleep(300 * time.Millisecond)

	src := filepath.Join(t.TempDir(), "new")
	want := []byte("#!/bin/sh\necho new\n")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	skipped, err := installBinary(src, dst)
	if err != nil || skipped {
		t.Fatalf("installBinary: skipped %v err %v", skipped, err)
	}
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("running process ended after the copy: %v", err)
	default:
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, want) {
		t.Errorf("target bytes %q", got)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", fi.Mode())
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("left files behind: %v", ents)
	}
}

func TestInstallBinarySameFileSkips(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "crosstalk")
	os.WriteFile(dst, []byte("x"), 0o755)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dst, link); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{dst, link} {
		if skipped, err := installBinary(src, dst); err != nil || !skipped {
			t.Errorf("installBinary(%s): skipped %v err %v", src, skipped, err)
		}
	}
}

func TestInstallMarketplaceOverride(t *testing.T) {
	home := installSandbox(t, "claude", "codex")
	t.Setenv("CROSSTALK_MARKETPLACE", "/scratch/mkt")
	if code, out := installRunCmd(t, "install", "--yes", "--bin-dir", filepath.Join(home, "bin")); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	b, _ := os.ReadFile(filepath.Join(home, "fake.log"))
	for _, c := range []string{"claude plugin marketplace add /scratch/mkt\n", "codex plugin marketplace add /scratch/mkt\n"} {
		if !strings.Contains(string(b), c) {
			t.Errorf("not run: %s", c)
		}
	}
	if strings.Contains(string(b), "GoodOlClint/CrossTalk") {
		t.Errorf("default marketplace used:\n%s", b)
	}
}

// installBrewKeg puts a copy of the sandbox binary at
// <root>/<prefix>/Cellar/crosstalk/0.1.4/bin/crosstalk, links
// <prefix>/bin/crosstalk to it when link is set, and makes the sandbox run
// as that binary (through the link when there is one). It returns the prefix.
func installBrewKeg(t *testing.T, prefix string, link bool) string {
	t.Helper()
	real, err := installExecutable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix = filepath.Join(root, prefix)
	keg := filepath.Join(prefix, "Cellar", "crosstalk", "0.1.4", "bin", "crosstalk")
	b, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(keg), 0o755)
	if err := os.WriteFile(keg, b, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := keg
	if link {
		exe = filepath.Join(prefix, "bin", "crosstalk")
		os.MkdirAll(filepath.Dir(exe), 0o755)
		if err := os.Symlink(keg, exe); err != nil {
			t.Fatal(err)
		}
	}
	installExecutable = func() (string, error) { return exe, nil }
	return prefix
}

func TestInstallHomebrewBinary(t *testing.T) {
	for _, rel := range []string{"opt/homebrew", "usr/local", "home/linuxbrew/.linuxbrew"} {
		t.Run(rel, func(t *testing.T) {
			home := installSandbox(t, "claude", "codex")
			prefix := installBrewKeg(t, rel, true)
			want := filepath.Join(prefix, "bin", "crosstalk")
			code, out := installRunCmd(t, "install", "--yes")
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if _, err := os.Stat(filepath.Join(home, ".local", "bin", "crosstalk")); !os.IsNotExist(err) {
				t.Errorf("~/.local/bin/crosstalk exists: %v", err)
			}
			if got := installRead(t, filepath.Join(home, ".claude.json")); !strings.Contains(got, `"command": "`+want+`"`) {
				t.Errorf(".claude.json does not name %s:\n%s", want, got)
			}
			if got := installRead(t, filepath.Join(home, ".codex", "config.toml")); !strings.Contains(got, `command = "`+want+`"`) {
				t.Errorf("config.toml does not name %s:\n%s", want, got)
			}
			for _, h := range []string{"claude", "codex"} {
				if l := installCheckLine(out, "K5", h); !strings.Contains(l, "PASS") {
					t.Errorf("K5 %s: %q", h, l)
				}
			}
		})
	}
}

func TestInstallNotHomebrewUsrLocal(t *testing.T) {
	home := installSandbox(t, "claude")
	real, _ := installExecutable()
	b, _ := os.ReadFile(real)
	root, _ := filepath.EvalSymlinks(t.TempDir())
	plain := filepath.Join(root, "usr", "local", "bin", "crosstalk")
	os.MkdirAll(filepath.Dir(plain), 0o755)
	if err := os.WriteFile(plain, b, 0o755); err != nil {
		t.Fatal(err)
	}
	installExecutable = func() (string, error) { return plain, nil }
	code, out := installRunCmd(t, "install", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "crosstalk")); err != nil {
		t.Errorf("binary not copied to ~/.local/bin: %v", err)
	}
	if got := installRead(t, filepath.Join(home, ".claude.json")); !strings.Contains(got, filepath.Join(home, ".local", "bin", "crosstalk")) {
		t.Errorf(".claude.json does not name ~/.local/bin/crosstalk:\n%s", got)
	}
}

func TestInstallHomebrewUnlinked(t *testing.T) {
	home := installSandbox(t, "claude")
	installBrewKeg(t, "opt/homebrew", false)
	code, out := installRunCmd(t, "install", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "crosstalk")); err != nil {
		t.Errorf("binary not copied to ~/.local/bin: %v", err)
	}
	if !strings.Contains(out, "not linked") {
		t.Errorf("no note about the unlinked keg:\n%s", out)
	}
}

func TestInstallHomebrewBinDirOverride(t *testing.T) {
	home := installSandbox(t, "claude")
	installBrewKeg(t, "opt/homebrew", true)
	bin := filepath.Join(home, "elsewhere")
	code, out := installRunCmd(t, "install", "--yes", "--bin-dir", bin)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(bin, "crosstalk")); err != nil {
		t.Errorf("binary not copied to --bin-dir: %v", err)
	}
	if got := installRead(t, filepath.Join(home, ".claude.json")); !strings.Contains(got, filepath.Join(bin, "crosstalk")) {
		t.Errorf(".claude.json does not name the --bin-dir copy:\n%s", got)
	}
}
