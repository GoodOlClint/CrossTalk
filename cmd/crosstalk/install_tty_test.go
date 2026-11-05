package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestIsTerminalDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("/dev/null reported as a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if isTerminal(r) {
		t.Error("pipe reported as a terminal")
	}
}

func TestInstallDevNullStdinExits2(t *testing.T) {
	bin := installBuild(t, ".")
	home := installSandbox(t, "claude", "codex")
	before := installSnapshot(t, home)
	for _, mode := range []string{"install", "uninstall"} {
		cmd := exec.Command(bin, mode, "--bin-dir", filepath.Join(home, "bin"))
		cmd.Env = os.Environ()
		null, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdin = null
		out, err := cmd.CombinedOutput()
		null.Close()
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 2 {
			t.Errorf("%s with stdin from %s: err %v, want exit 2\n%s", mode, os.DevNull, err, out)
		}
	}
	if after := installSnapshot(t, home); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Errorf("HOME changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestInstallRemovePathDeletesEmptyFiles(t *testing.T) {
	home := t.TempDir()
	files := installPathFiles("darwin", home)
	line := installPathLine(filepath.Join(home, "bin"))
	if err := installAddPath(files, line); err != nil {
		t.Fatal(err)
	}
	if err := installRemovePath(files, line); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s still exists after remove: %v", f, err)
		}
	}
	if err := os.WriteFile(files[0], []byte("# keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installAddPath(files, line); err != nil {
		t.Fatal(err)
	}
	if err := installRemovePath(files, line); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil || string(b) != "# keep\n" {
		t.Errorf("%s = %q, %v; want %q", files[0], b, err, "# keep\n")
	}
	if _, err := os.Stat(files[1]); !os.IsNotExist(err) {
		t.Errorf("%s still exists: %v", files[1], err)
	}
}
