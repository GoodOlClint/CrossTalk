package shim

import (
	"go/build"
	"path/filepath"
	"strings"
	"testing"
)

// ADR 0008: only cmd/crosstalk imports a concrete harness package. Packages
// are read in-process so the test cache sees every file that decides it.
func TestSharedCodeImportsNoHarnessPackage(t *testing.T) {
	const mod = "github.com/GoodOlClint/crosstalk/"
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"internal/shim", "internal/mcpserver", "internal/a2a", "internal/wire"} {
		seen := map[string]bool{}
		var walk func(rel string, via []string)
		walk = func(rel string, via []string) {
			if seen[rel] {
				return
			}
			seen[rel] = true
			if strings.HasPrefix(rel, "internal/harness/") {
				t.Errorf("%s imports %s via %v", pkg, rel, via)
				return
			}
			p, err := build.ImportDir(filepath.Join(root, rel), 0)
			if err != nil {
				t.Fatalf("%s: %v", rel, err)
			}
			for _, imp := range p.Imports {
				if dep, ok := strings.CutPrefix(imp, mod); ok {
					walk(dep, append(via, rel))
				}
			}
		}
		walk(pkg, nil)
	}
}
