package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

type span struct{ start, end int }

// jsoncClean blanks comments and trailing commas in src with spaces, keeping
// every offset, so spans found in clean index src. Reading only: a write
// never goes through clean.
func jsoncClean(src []byte) (clean []byte, comments bool) {
	clean = bytes.Clone(src)
	blank := func(from, to int) {
		for i := from; i < to; i++ {
			if clean[i] != '\n' {
				clean[i] = ' '
			}
		}
	}
	for i := 0; i < len(clean); {
		switch {
		case clean[i] == '"':
			i++
			for i < len(clean) && clean[i] != '"' {
				if clean[i] == '\\' {
					i++
				}
				i++
			}
			i++
		case clean[i] == '/' && i+1 < len(clean) && clean[i+1] == '/':
			j := bytes.IndexByte(clean[i:], '\n')
			if j < 0 {
				j = len(clean) - i
			}
			blank(i, i+j)
			comments = true
			i += j
		case clean[i] == '/' && i+1 < len(clean) && clean[i+1] == '*':
			j := bytes.Index(clean[i+2:], []byte("*/"))
			end := len(clean)
			if j >= 0 {
				end = i + 2 + j + 2
			}
			blank(i, end)
			comments = true
			i = end
		default:
			i++
		}
	}
	var comma = -1
	for i := 0; i < len(clean); i++ {
		switch c := clean[i]; {
		case c == '"':
			i++
			for i < len(clean) && clean[i] != '"' {
				if clean[i] == '\\' {
					i++
				}
				i++
			}
			comma = -1
		case c == ',':
			comma = i
		case c == '}' || c == ']':
			if comma >= 0 {
				clean[comma] = ' '
			}
			comma = -1
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
		default:
			comma = -1
		}
	}
	return clean, comments
}

// memberSpan finds the object member at path in b, a JSON value that starts
// at offset base of the file, and returns the bytes from its key through its
// value.
func memberSpan(b []byte, base int, path []string) (span, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return span{}, false
	}
	for dec.More() {
		before := int(dec.InputOffset())
		kt, err := dec.Token()
		if err != nil {
			return span{}, false
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return span{}, false
		}
		end := int(dec.InputOffset())
		if kt != path[0] {
			continue
		}
		ks := before
		for ks < len(b) && (b[ks] == ' ' || b[ks] == '\t' || b[ks] == '\n' || b[ks] == '\r' || b[ks] == ',') {
			ks++
		}
		if len(path) == 1 {
			return span{base + ks, base + end}, true
		}
		return memberSpan(raw, base+end-len(raw), path[1:])
	}
	return span{}, false
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// cutMembers removes the members at spans from src, with the comma that
// separated each from its neighbour and the whole lines they sat on. The
// result is not checked here.
func cutMembers(src []byte, spans []span) []byte {
	type cut struct{ from, to int }
	var cuts []cut
	for _, sp := range spans {
		s, e := sp.start, sp.end
		j := e
		for j < len(src) && isSpace(src[j]) {
			j++
		}
		if j < len(src) && src[j] == ',' {
			e = j + 1
		} else {
			k := s - 1
			for k >= 0 && isSpace(src[k]) {
				k--
			}
			if k >= 0 && src[k] == ',' {
				cuts = append(cuts, cut{k, k + 1})
			}
		}
		ls := s
		for ls > 0 && src[ls-1] != '\n' {
			ls--
		}
		if len(bytes.TrimSpace(src[ls:s])) == 0 {
			s = ls
		}
		le := e
		for le < len(src) && src[le] != '\n' && isSpace(src[le]) {
			le++
		}
		if le >= len(src) || src[le] == '\n' {
			if le < len(src) {
				le++
			}
			e = le
		}
		cuts = append(cuts, cut{s, e})
	}
	slices.SortFunc(cuts, func(a, b cut) int { return b.from - a.from })
	out := bytes.Clone(src)
	for _, c := range cuts {
		out = append(out[:c.from], out[c.to:]...)
	}
	return out
}

// lineSpan widens sp to whole lines of src and returns them with the first
// line number.
func lineSpan(src []byte, sp span) (string, int) {
	s, e := sp.start, sp.end
	for s > 0 && src[s-1] != '\n' {
		s--
	}
	for e < len(src) && src[e] != '\n' {
		e++
	}
	return string(src[s:e]), bytes.Count(src[:s], []byte{'\n'}) + 1
}

type entry struct {
	shape    string
	file     string
	command  []string
	env      map[string]string
	path     []string
	disabled bool
}

const (
	shapeV2 = "mcp.servers.crosstalk"
	shapeV1 = "mcp.crosstalk"
)

type config struct {
	path     string
	src      []byte
	clean    []byte
	comments bool
	entries  []entry
}

func parseConfig(path string, src []byte) (*config, error) {
	c := &config{path: path, src: src}
	c.clean, c.comments = jsoncClean(src)
	if len(bytes.TrimSpace(c.clean)) == 0 {
		return c, nil
	}
	fail := func(f string, a ...any) (*config, error) {
		return nil, fmt.Errorf("%s: %s", path, fmt.Sprintf(f, a...))
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(c.clean, &root); err != nil {
		return fail("not a JSON object: %v", err)
	}
	raw, ok := root["mcp"]
	if !ok {
		return c, nil
	}
	var mcp map[string]json.RawMessage
	if err := json.Unmarshal(raw, &mcp); err != nil {
		return fail(`"mcp" is not an object`)
	}
	read := func(shape string, p []string, raw json.RawMessage) error {
		var e struct {
			Command     json.RawMessage   `json:"command"`
			Environment map[string]string `json:"environment"`
			Enabled     *bool             `json:"enabled"`
			Disabled    bool              `json:"disabled"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf(`"%s" is not a readable server entry`, shape)
		}
		var cmd []string
		if err := json.Unmarshal(e.Command, &cmd); err != nil {
			return fmt.Errorf(`"%s" has no readable command array`, shape)
		}
		if len(cmd) == 0 {
			return fmt.Errorf(`"%s" has an empty command`, shape)
		}
		off := e.Disabled || (e.Enabled != nil && !*e.Enabled)
		c.entries = append(c.entries, entry{shape: shape, file: path, command: cmd, env: e.Environment, path: p, disabled: off})
		return nil
	}
	if raw, ok := mcp["servers"]; ok {
		var servers map[string]json.RawMessage
		if err := json.Unmarshal(raw, &servers); err != nil {
			return fail(`"mcp.servers" is not an object`)
		}
		if raw, ok := servers["crosstalk"]; ok {
			if err := read(shapeV2, []string{"mcp", "servers", "crosstalk"}, raw); err != nil {
				return fail("%v", err)
			}
		}
	}
	if raw, ok := mcp["crosstalk"]; ok {
		if err := read(shapeV1, []string{"mcp", "crosstalk"}, raw); err != nil {
			return fail("%v", err)
		}
	}
	return c, nil
}
