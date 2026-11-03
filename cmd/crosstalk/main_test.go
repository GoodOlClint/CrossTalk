package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	cases := []struct {
		args []string
		code int
		out  string
		err  string
	}{
		{nil, 2, "", "usage: crosstalk"},
		{[]string{"help"}, 0, "usage: crosstalk", ""},
		{[]string{"capture"}, 2, "", "usage: crosstalk capture"},
		{[]string{"bogus"}, 2, "", "unknown command"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if got := run(c.args, &out, &errb); got != c.code {
			t.Errorf("run(%v) = %d, want %d", c.args, got, c.code)
		}
		if !strings.Contains(out.String(), c.out) || !strings.Contains(errb.String(), c.err) {
			t.Errorf("run(%v) stdout=%q stderr=%q", c.args, out.String(), errb.String())
		}
	}
}
