package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPathStepMarkedAndIdempotent(t *testing.T) {
	for goos, want := range map[string][]string{"darwin": {".zprofile", ".zshenv"}} {
		home := t.TempDir()
		files := installPathFiles(goos, home)
		if len(files) != len(want) {
			t.Fatalf("%s: files %v, want %v", goos, files, want)
		}
		for i, f := range files {
			if f != filepath.Join(home, want[i]) {
				t.Errorf("%s: file %s, want %s", goos, f, want[i])
			}
		}
		installWrite(t, files[0], "# mine\n")
		line := installPathLine("/opt/x/bin")
		for range 2 {
			if err := installAddPath(files, line); err != nil {
				t.Fatal(err)
			}
		}
		for _, f := range files {
			if n := strings.Count(installRead(t, f), line+"\n"); n != 1 {
				t.Errorf("%s: %d marked lines", f, n)
			}
		}
		if !strings.HasPrefix(installRead(t, files[0]), "# mine\n") {
			t.Errorf("%s lost its content", files[0])
		}
		installWrite(t, files[1], installRead(t, files[1])+"export PATH=\"/elsewhere:$PATH\" # crosstalk\n")
		if err := installRemovePath(files, line); err != nil {
			t.Fatal(err)
		}
		if got := installRead(t, files[0]); got != "# mine\n" {
			t.Errorf("%s after remove: %q", files[0], got)
		}
		if got := installRead(t, files[1]); got != "export PATH=\"/elsewhere:$PATH\" # crosstalk\n" {
			t.Errorf("%s after remove: %q", files[1], got)
		}
	}
}
