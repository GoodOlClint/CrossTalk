package appserver

import (
	"path/filepath"
	"testing"
)

func TestDefaultControlSocketFollowsCodexHome(t *testing.T) {
	home, ch := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", ch)
	got, err := DefaultControlSocket()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(ch, "app-server-control", "app-server-control.sock"); got != want {
		t.Errorf("DefaultControlSocket = %q, want %q", got, want)
	}
}

func TestDefaultControlSocketUnsetCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	got, err := DefaultControlSocket()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".codex", "app-server-control", "app-server-control.sock"); got != want {
		t.Errorf("DefaultControlSocket = %q, want %q", got, want)
	}
}

func TestDefaultControlSocketRelativeCodexHome(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", "profile")
	got, err := DefaultControlSocket()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) || filepath.Base(filepath.Dir(filepath.Dir(got))) != "profile" {
		t.Errorf("DefaultControlSocket = %q, want an absolute path under profile", got)
	}
}
