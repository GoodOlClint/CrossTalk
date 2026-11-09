package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/mcpserver"
)

func withRunningVersion(t *testing.T, v string) {
	t.Helper()
	old := mcpserver.Version
	mcpserver.Version = v
	t.Cleanup(func() { mcpserver.Version = old })
}

// fakeVersionBin writes a script that logs each run to <path>.runs and
// prints out; it returns the log path.
func fakeVersionBin(t *testing.T, path, out string) string {
	t.Helper()
	log := path + ".runs"
	installWrite(t, path, "#!/bin/sh\necho run >> '"+log+"'\necho '"+out+"'\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return log
}

func runCount(log string) int {
	b, _ := os.ReadFile(log)
	return strings.Count(string(b), "run")
}

func TestEnableRefusesOlderBinary(t *testing.T) {
	home, rec := unitSandbox(t, "linux")
	bin := filepath.Join(home, ".local", "bin", "crosstalk")
	withRunningVersion(t, "0.2.0")
	fakeVersionBin(t, bin, "0.1.5")
	code, out := enableCmd(t, "--bind", "127.0.0.1", "--yes")
	if code != 1 || !strings.Contains(out, bin) || !strings.Contains(out, "crosstalk reinstall") {
		t.Errorf("enable with an older binary: exit %d, want 1 naming %s and crosstalk reinstall\n%s", code, bin, out)
	}
	if _, err := os.Stat(remoteUnitPath(home)); !os.IsNotExist(err) {
		t.Errorf("unit written despite refusal: %v", err)
	}
	if c := rec.all(); len(c) != 0 {
		t.Errorf("commands ran: %v", c)
	}
}

func TestEnableAcceptsEqualAndNewerBinary(t *testing.T) {
	for _, got := range []string{"0.2.0", "0.2.1", "0.10.0"} {
		t.Run(got, func(t *testing.T) {
			home, _ := unitSandbox(t, "linux")
			bin := filepath.Join(home, ".local", "bin", "crosstalk")
			withRunningVersion(t, "0.2.0")
			fakeVersionBin(t, bin, got)
			code, out := enableCmd(t, "--bind", "127.0.0.1", "--yes")
			if code != 0 {
				t.Errorf("enable with %s against running 0.2.0: exit %d\n%s", got, code, out)
			}
			if _, err := os.Stat(remoteUnitPath(home)); err != nil {
				t.Errorf("unit not written: %v", err)
			}
		})
	}
}

func TestCheckUnitBinOrdering(t *testing.T) {
	for _, c := range []struct {
		running, target string
		ok              bool
	}{
		{"0.1.9", "0.1.10", true},
		{"0.1.10", "0.1.9", false},
		{"0.2.0", "0.2.0-rc.1", false},
		{"0.2.0-rc.1", "0.2.0", true},
		{"0.2.0-rc.1", "0.2.0-rc.1", true},
		{"0.2.0", "v0.2.0", true},
	} {
		dir := t.TempDir()
		bin := filepath.Join(dir, "crosstalk")
		fakeVersionBin(t, bin, c.target)
		withRunningVersion(t, c.running)
		if err := checkUnitBin(context.Background(), bin); (err == nil) != c.ok {
			t.Errorf("running %s, target %s: err %v, want ok=%v", c.running, c.target, err, c.ok)
		}
	}
}

func TestCheckUnitBinRefusesUnparseableVersion(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "crosstalk")
	withRunningVersion(t, "0.2.0")
	for _, out := range []string{"garbage", "", "1.2", "usage: crosstalk"} {
		fakeVersionBin(t, bin, out)
		err := checkUnitBin(context.Background(), bin)
		if err == nil || !strings.Contains(err.Error(), "crosstalk reinstall") || !strings.Contains(err.Error(), bin) {
			t.Errorf("version output %q: err %v, want the update message naming %s", out, err, bin)
		}
	}
}

func TestCheckUnitBinSkipsRunningBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "crosstalk")
	log := fakeVersionBin(t, bin, "0.0.1")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	old := installExecutable
	installExecutable = func() (string, error) { return bin, nil }
	t.Cleanup(func() { installExecutable = old })
	withRunningVersion(t, "0.2.0")
	for _, p := range []string{bin, link} {
		if err := checkUnitBin(context.Background(), p); err != nil {
			t.Errorf("running binary via %s: %v", p, err)
		}
	}
	if n := runCount(log); n != 0 {
		t.Errorf("running binary executed %d times, want 0", n)
	}
}

func TestCheckUnitBinChecksOtherBinary(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "running")
	fakeVersionBin(t, running, "0.2.0")
	other := filepath.Join(dir, "other")
	log := fakeVersionBin(t, other, "0.0.1")
	old := installExecutable
	installExecutable = func() (string, error) { return running, nil }
	t.Cleanup(func() { installExecutable = old })
	withRunningVersion(t, "0.2.0")
	if err := checkUnitBin(context.Background(), other); err == nil {
		t.Error("an older binary other than the running one passed")
	}
	if n := runCount(log); n != 1 {
		t.Errorf("other binary executed %d times, want 1", n)
	}
}

func TestCheckUnitBinHomebrewMessage(t *testing.T) {
	prefix := t.TempDir()
	keg := filepath.Join(prefix, "Cellar", "crosstalk", "0.1.5", "bin", "crosstalk")
	fakeVersionBin(t, keg, "0.1.5")
	link := filepath.Join(prefix, "bin", "crosstalk")
	os.MkdirAll(filepath.Dir(link), 0o755)
	if err := os.Symlink(keg, link); err != nil {
		t.Fatal(err)
	}
	withRunningVersion(t, "0.2.0")
	err := checkUnitBin(context.Background(), link)
	if err == nil || !strings.Contains(err.Error(), "brew upgrade crosstalk") || strings.Contains(err.Error(), "crosstalk reinstall") {
		t.Errorf("homebrew binary: err %v, want brew upgrade crosstalk only", err)
	}
}

func TestCheckUnitBinPassesOwnBuild(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "crosstalk")
	src := installBuild(t, ".")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkUnitBin(context.Background(), bin); err != nil {
		t.Errorf("a build of this tree fails its own check: %v", err)
	}
}
