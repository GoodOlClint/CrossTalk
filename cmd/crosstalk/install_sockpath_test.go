package main

import (
	"path/filepath"
	"strings"
	"testing"
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
