package msg

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Kind string

const (
	Inform  Kind = "inform"
	Command Kind = "command"
	Ask     Kind = "ask"
)

type Delivery string

const (
	Immediate Delivery = "immediate"
	NextTurn  Delivery = "next_turn"
)

const MaxHops = 32

type Part struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type Message struct {
	MessageID       string    `json:"message_id"`
	InReplyTo       *string   `json:"in_reply_to"`
	ContextID       string    `json:"context_id"`
	TaskID          *string   `json:"task_id"`
	From            string    `json:"from"`
	To              string    `json:"to"`
	Kind            Kind      `json:"kind"`
	Delivery        Delivery  `json:"delivery"`
	DeadlineSeconds int       `json:"deadline_seconds,omitempty"`
	HopChain        []string  `json:"hop_chain"`
	SentAt          time.Time `json:"sent_at"`
	Parts           []Part    `json:"parts"`
	// Inbox is the role inbox the message is addressed to, and InboxCopy marks
	// a watcher's copy. A sender sets them; a receiver reads them from the
	// A2A metadata, never from this struct.
	Inbox     string `json:"inbox,omitempty"`
	InboxCopy bool   `json:"inbox_copy,omitempty"`
}

// Text is the text parts joined by newlines.
func (m Message) Text() string {
	texts := make([]string, 0, len(m.Parts))
	for _, p := range m.Parts {
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n")
}

var (
	ErrUnknownKind  = errors.New("unknown kind")
	ErrHopRepeat    = errors.New("hop_chain repeat")
	ErrHopOverflow  = errors.New("hop_chain exceeds 32 entries")
	ErrInvalidField = errors.New("invalid field")

	hop24RE = regexp.MustCompile(`^[0-9a-f]{24}$`)
)

func invalid(field, why string) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalidField, field, why)
}

func (k Kind) Valid() bool { return k == Inform || k == Command || k == Ask }

func (d Delivery) Valid() bool { return d == Immediate || d == NextTurn }

func (m *Message) Validate() error {
	if !IsUUIDv7(m.MessageID) {
		return invalid("message_id", "not a uuidv7")
	}
	if m.InReplyTo != nil && !IsUUIDv7(*m.InReplyTo) {
		return invalid("in_reply_to", "not a uuidv7")
	}
	if m.ContextID == "" {
		return invalid("context_id", "empty")
	}
	if m.TaskID != nil && *m.TaskID == "" {
		return invalid("task_id", "empty")
	}
	if m.From == "" {
		return invalid("from", "empty")
	}
	if m.To == "" {
		return invalid("to", "empty")
	}
	if !m.Kind.Valid() {
		return fmt.Errorf("%w: %q", ErrUnknownKind, m.Kind)
	}
	if !m.Delivery.Valid() {
		return invalid("delivery", string(m.Delivery))
	}
	if m.DeadlineSeconds < 0 {
		return invalid("deadline_seconds", "negative")
	}
	if m.DeadlineSeconds != 0 && m.Kind != Ask {
		return invalid("deadline_seconds", "only valid on ask")
	}
	if err := CheckHopChain(m.HopChain); err != nil {
		return err
	}
	if m.SentAt.IsZero() {
		return invalid("sent_at", "zero")
	}
	if len(m.Parts) == 0 {
		return invalid("parts", "empty")
	}
	for i, p := range m.Parts {
		if p.Type != "text" {
			return invalid(fmt.Sprintf("parts[%d].type", i), p.Type)
		}
	}
	return nil
}

// Hop is a hop_chain entry in Claude's convention: the first 24 hex of sha256(id).
func Hop(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:12])
}

func CheckHopChain(chain []string) error {
	if len(chain) > MaxHops {
		return ErrHopOverflow
	}
	seen := make(map[string]struct{}, len(chain))
	for i, h := range chain {
		if !hop24RE.MatchString(h) {
			return invalid(fmt.Sprintf("hop_chain[%d]", i), "not 24 lowercase hex")
		}
		if _, ok := seen[h]; ok {
			return ErrHopRepeat
		}
		seen[h] = struct{}{}
	}
	return nil
}
