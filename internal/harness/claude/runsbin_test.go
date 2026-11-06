package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

func entryEnv(t *testing.T, scope, bin, command string) harness.InstallEnv {
	t.Helper()
	home := t.TempDir()
	e := map[string]any{"command": command, "args": []string{"mcp"}}
	var cj map[string]any
	if scope == "user" {
		cj = map[string]any{"mcpServers": map[string]any{"crosstalk": e}}
	} else {
		cj = map[string]any{"projects": map[string]any{filepath.Join(home, "proj"): map[string]any{"mcpServers": map[string]any{"crosstalk": e}}}}
	}
	b, _ := json.Marshal(cj)
	writeFile(t, filepath.Join(home, ".claude.json"), string(b))
	return harness.InstallEnv{Home: home, Bin: bin}
}

func TestEntryRunsBinary(t *testing.T) {
	h := &Harness{}
	cases := map[string]struct {
		scope string
		cmd   func(t *testing.T, bin string) string
		want  bool
	}{
		"local symlink": {"local", func(t *testing.T, bin string) string {
			l := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(bin, l); err != nil {
				t.Fatal(err)
			}
			return l
		}, true},
		"local unclean": {"local", func(t *testing.T, bin string) string {
			return filepath.Dir(bin) + "/sub/../crosstalk"
		}, true},
		"local other binary": {"local", func(t *testing.T, bin string) string {
			o := filepath.Join(t.TempDir(), "crosstalk")
			writeFile(t, o, "y")
			return o
		}, false},
		"user symlink": {"user", func(t *testing.T, bin string) string {
			l := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(bin, l); err != nil {
				t.Fatal(err)
			}
			return l
		}, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "crosstalk")
			writeFile(t, bin, "x")
			env := entryEnv(t, c.scope, bin, c.cmd(t, bin))
			if c.scope == "user" {
				got, err := h.Registered(env)
				if err != nil || got != c.want {
					t.Fatalf("Registered = %v, %v; want %v", got, err, c.want)
				}
				return
			}
			left, err := h.Leftover(env, false)
			if err != nil || (left != "") != c.want {
				t.Fatalf("Leftover = %q, %v; want kept=%v", left, err, c.want)
			}
		})
	}
}
