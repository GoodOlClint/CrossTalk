package main

import (
	"archive/tar"
	"compress/gzip"
	"debug/elf"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func releaseRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func releaseScript(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(filepath.Join("scripts", "release-build.sh"), args...)
	cmd.Dir = releaseRepoRoot(t)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestReleaseBuildLinux(t *testing.T) {
	if _, err := exec.LookPath("shasum"); err != nil {
		t.Skip("shasum not on PATH")
	}
	dir := t.TempDir()
	if out, err := releaseScript(t, nil, "build-linux", dir); err != nil {
		t.Fatalf("build-linux: %v\n%s", err, out)
	}
	const tag = "v9.9.9"
	if out, err := releaseScript(t, nil, "package-linux", dir, tag); err != nil {
		t.Fatalf("package-linux: %v\n%s", err, out)
	}
	for arch, machine := range map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64} {
		name := "crosstalk-" + tag + "-linux-" + arch + ".tar.gz"
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(gz)
		var entries []string
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			entries = append(entries, h.Name)
			if h.Name != "crosstalk" {
				continue
			}
			if h.Mode&0o777 != 0o755 {
				t.Errorf("%s: mode %o, want 755", arch, h.Mode&0o777)
			}
			bin, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "crosstalk-"+arch)
			if err := os.WriteFile(path, bin, 0o755); err != nil {
				t.Fatal(err)
			}
			ef, err := elf.Open(path)
			if err != nil {
				t.Fatalf("%s: not ELF: %v", arch, err)
			}
			if ef.Machine != machine {
				t.Errorf("%s: machine %v, want %v", arch, ef.Machine, machine)
			}
			ef.Close()
		}
		f.Close()
		if len(entries) != 1 || entries[0] != "crosstalk" {
			t.Errorf("%s: entries %v, want [crosstalk]", arch, entries)
		}
		sum := exec.Command("shasum", "-a", "256", "-c", name+".sha256")
		sum.Dir = dir
		if out, err := sum.CombinedOutput(); err != nil || !strings.Contains(string(out), "OK") {
			t.Errorf("%s: checksum: %v\n%s", arch, err, out)
		}
	}
}
