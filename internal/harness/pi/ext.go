// Package pi is the Pi harness.
package pi

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed ext/index.ts
var source string

const marker = "// crosstalk-owned: written by `crosstalk install pi`"

const (
	refreshOn  = "\nconst noRefresh: boolean = false;\n"
	refreshOff = "\nconst noRefresh: boolean = true;\n"
)

// Fill returns the extension file to write over prev, the current file or nil.
// A no-auto-refresh choice already in prev is kept.
func Fill(bin, version string, noAutoRefresh bool, prev []byte) []byte {
	quote := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	pairs := []string{
		"// __CROSSTALK_MARKER__", marker,
		`"__CROSSTALK_BIN__"`, quote(bin),
		`"__CROSSTALK_VERSION__"`, quote(version),
	}
	if noAutoRefresh || bytes.Contains(prev, []byte(refreshOff)) {
		pairs = append(pairs, refreshOn, refreshOff)
	}
	return []byte(strings.NewReplacer(pairs...).Replace(source))
}
