package main

import (
	"archive/tar"
	"compress/gzip"
	"debug/elf"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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

func TestReleaseWorkflowLinuxOutsideSigning(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(releaseRepoRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	steps := strings.Split(string(raw), "\n      - ")[1:]
	signing := regexp.MustCompile(`\b(codesign|notarytool|security|lipo)\b`)
	var publish string
	linux := 0
	for _, s := range steps {
		if !strings.Contains(s, "dist-linux") {
			continue
		}
		linux++
		if m := signing.FindString(s); m != "" {
			t.Errorf("step names dist-linux and runs %q:\n%s", m, s)
		}
		if strings.Contains(s, "gh release") {
			publish = s
		}
	}
	if linux < 3 {
		t.Errorf("%d steps name dist-linux, want build, package and publish", linux)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		for _, suffix := range []string{".tar.gz", ".tar.gz.sha256"} {
			want := `dist-linux/crosstalk-$TAG-linux-` + arch + suffix + `"`
			if !strings.Contains(publish, want) {
				t.Errorf("publish files=(...) lacks %s", want)
			}
		}
	}
}

func TestCheckIdentity(t *testing.T) {
	const want = "Developer ID Application: Test (ABCDE12345)"
	line := func(n int, name string) string {
		return "  " + strconv.Itoa(n) + ") 0123456789ABCDEF0123456789ABCDEF01234567 \"" + name + "\"\n"
	}
	cases := []struct {
		name   string
		output string
		ok     bool
	}{
		{"none", "     0 valid identities found\n", false},
		{"one", line(1, want) + "     1 valid identities found\n", true},
		{"two", line(1, want) + line(2, want) + "     2 valid identities found\n", false},
		{"near miss", line(1, want+" copy") + "     1 valid identities found\n", false},
		{"one among others", line(1, "Other") + line(2, want) + "     2 valid identities found\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin := t.TempDir()
			fake := "#!/bin/sh\ncat <<'EOF'\n" + c.output + "EOF\n"
			if err := os.WriteFile(filepath.Join(bin, "security"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := releaseScript(t, []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "APPLE_CODESIGN_IDENTITY=" + want}, "check-identity", "unused.keychain-db")
			if (err == nil) != c.ok {
				t.Errorf("err = %v, want ok=%v\n%s", err, c.ok, out)
			}
		})
	}
}
