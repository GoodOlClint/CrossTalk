package udsingress

import (
	"encoding/json"
	"strings"
)

// Frame is a parsed user line. Attrs are the envelope attributes: display
// only, never identity or authority (§7).
type Frame struct {
	Body     string
	Envelope bool
	Attrs    map[string]string
	// Kind is the kind the frame asked for, from the envelope kind attribute
	// or a crosstalk.kind field; empty when it asked for none.
	Kind string
}

type kindField struct {
	Kind string `json:"kind"`
}

type userLine struct {
	Type      string          `json:"type"`
	Message   json.RawMessage `json:"message"`
	Crosstalk *kindField      `json:"crosstalk"`
	DotKind   string          `json:"crosstalk.kind"`
}

type userMsg struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Crosstalk *kindField      `json:"crosstalk"`
	DotKind   string          `json:"crosstalk.kind"`
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func ParseUserFrame(line []byte) (Frame, error) {
	var u userLine
	if err := json.Unmarshal(line, &u); err != nil || u.Type != "user" || len(u.Message) == 0 {
		return Frame{}, errBadFrame
	}
	var m userMsg
	if err := json.Unmarshal(u.Message, &m); err != nil {
		return Frame{}, errBadFrame
	}
	content, err := contentText(m.Content)
	if err != nil {
		return Frame{}, err
	}
	f, err := ParseContent(content)
	if err != nil {
		return Frame{}, err
	}
	for _, k := range []string{u.DotKind, m.DotKind, kindOf(u.Crosstalk), kindOf(m.Crosstalk)} {
		if k == "" {
			continue
		}
		if f.Kind == "" {
			f.Kind = k
		} else if k != f.Kind {
			f.Kind = "conflicting"
		}
	}
	return f, nil
}

func kindOf(k *kindField) string {
	if k == nil {
		return ""
	}
	return k.Kind
}

func contentText(raw json.RawMessage) (string, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return "", errNoContent
		}
		return s, nil
	}
	var bs []block
	if err := json.Unmarshal(raw, &bs); err != nil {
		return "", errBadFrame
	}
	var texts []string
	for _, b := range bs {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	if len(texts) == 0 {
		return "", errNoContent
	}
	return strings.Join(texts, "\n"), nil
}

const (
	openTag  = "<cross-session-message"
	closeTag = "</cross-session-message>"
)

// ParseContent reads C as plain text or one outer <cross-session-message>
// envelope. Only the outermost envelope is parsed; anything inside the body,
// including a nested envelope, is body text. Content that opens with the tag
// but does not parse is an error, so a malformed envelope cannot hide a kind.
func ParseContent(c string) (Frame, error) {
	t := strings.TrimSpace(c)
	if !strings.HasPrefix(t, openTag) {
		return Frame{Body: c}, nil
	}
	if !strings.HasSuffix(t, closeTag) {
		return Frame{}, errBadFrame
	}
	end := tagEnd(t, len(openTag))
	if end < 0 || end > len(t)-len(closeTag) {
		return Frame{}, errBadFrame
	}
	attrs, ok := parseAttrs(t[len(openTag):end])
	if !ok {
		return Frame{}, errBadFrame
	}
	body := t[end+1 : len(t)-len(closeTag)]
	body = strings.TrimPrefix(body, "\n")
	body = strings.TrimSuffix(body, "\n")
	return Frame{Body: strings.ReplaceAll(body, "&lt;", "<"), Envelope: true, Attrs: attrs, Kind: attrs["kind"]}, nil
}

func tagEnd(s string, from int) int {
	quoted := false
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case '>':
			if !quoted {
				return i
			}
		}
	}
	return -1
}

var attrUnescaper = strings.NewReplacer("&quot;", `"`, "&lt;", "<", "&gt;", ">", "&amp;", "&")

func parseAttrs(s string) (map[string]string, bool) {
	attrs := map[string]string{}
	for {
		s = strings.TrimLeft(s, " \t\n")
		if s == "" {
			return attrs, true
		}
		eq := strings.Index(s, `="`)
		if eq <= 0 || strings.ContainsAny(s[:eq], " \t\n\"") {
			return nil, false
		}
		name := s[:eq]
		rest := s[eq+2:]
		q := strings.IndexByte(rest, '"')
		if q < 0 {
			return nil, false
		}
		attrs[name] = attrUnescaper.Replace(rest[:q])
		s = rest[q+1:]
	}
}
