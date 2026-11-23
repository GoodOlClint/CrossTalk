package opencode

import (
	"errors"
	"testing"
)

func TestCheckVersion(t *testing.T) {
	for _, tc := range []struct {
		v    string
		warn bool
		ok   bool
	}{
		{"2.4.0", false, true},
		{"2.4.1", false, true},
		{"2.9.0", true, true},
		{"2.0", false, false},
		{"2.4.invalid", false, false},
		{"2.4.0-PW-s3cr3t-Zk9Qx", false, false},
		{"02.4.0", false, false},
		{"2.4.0 ", false, false},
		{"1.4.2", false, false},
		{"3.0.0", false, false},
		{"0.9.0", false, false},
		{"", false, false},
		{"2", false, false},
		{"v2.4.0", false, false},
		{"two.four", false, false},
	} {
		warn, err := CheckVersion(tc.v)
		if (err == nil) != tc.ok {
			t.Errorf("%q: err %v want ok=%v", tc.v, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrUntestedVersion) {
			t.Errorf("%q: %v does not wrap ErrUntestedVersion", tc.v, err)
		}
		if err != nil {
			noPassword(t, "version error", err.Error())
		}
		if (warn != "") != tc.warn {
			t.Errorf("%q: warn %q want %v", tc.v, warn, tc.warn)
		}
	}
	if len(TestedMajors) != 1 || TestedMajors[0] != 2 {
		t.Errorf("TestedMajors %v", TestedMajors)
	}
}
