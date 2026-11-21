package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/registry"
	"github.com/GoodOlClint/crosstalk/internal/udsingress"
)

func TestSockPathCheck_LongHome_ExpectWarn(t *testing.T) {
	r := installSockPathCheck(filepath.Join(t.TempDir(), strings.Repeat("h", 120)))
	if r.ID != "K12" || r.OK || !r.WarnOnly || !strings.Contains(r.Detail, "too long") {
		t.Fatalf("got %+v, want K12 warn naming too long", r)
	}
}

func TestSockPathCheck_ShortHome_ExpectPass(t *testing.T) {
	if r := installSockPathCheck("/h"); !r.OK {
		t.Fatalf("got %+v, want pass", r)
	}
}

func TestSockPathCheck_Boundary_ExpectMatchesRegistryPath(t *testing.T) {
	fixed := len((&registry.Registry{Dir: filepath.Join("/", ".local", "state", "crosstalk")}).UDSPath(strings.Repeat("0", 16)))
	for _, c := range []struct {
		extra int
		ok    bool
	}{{udsingress.MaxSockPath - fixed, true}, {udsingress.MaxSockPath - fixed + 1, false}} {
		home := "/" + strings.Repeat("h", c.extra-1)
		reg := &registry.Registry{Dir: filepath.Join(home, ".local", "state", "crosstalk")}
		if got := len(reg.UDSPath(strings.Repeat("0", 16))); (got <= udsingress.MaxSockPath) != c.ok {
			t.Fatalf("setup: path len %d", got)
		}
		if r := installSockPathCheck(home); r.OK != c.ok {
			t.Errorf("home len %d: OK=%v, want %v", len(home), r.OK, c.ok)
		}
	}
}
