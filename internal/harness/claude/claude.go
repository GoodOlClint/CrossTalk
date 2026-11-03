package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/procstart"
)

const (
	DefaultDialTimeout = 2 * time.Second
	DefaultAckTimeout  = 10 * time.Second
	maxAckLine         = 64 << 10
)

var (
	ErrNoSession    = errors.New("claude: no such session")
	ErrAmbiguous    = errors.New("claude: session id matches more than one live session file")
	ErrBadFrom      = errors.New("claude: from address does not match the envelope grammar")
	ErrStaleBinding = errors.New("claude: session file no longer matches the binding")
	ErrNoKey        = errors.New("claude: no unique session key file")

	fromRE = regexp.MustCompile(`^(uds|bridge|did):[A-Za-z0-9%:_/.\-]{1,200}$`)
)

// Session is the subset of ~/.claude/sessions/<pid>.json the shim reads.
type Session struct {
	PID                 int    `json:"pid"`
	SessionID           string `json:"sessionId"`
	Cwd                 string `json:"cwd"`
	Name                string `json:"name"`
	Status              string `json:"status"`
	MessagingSocketPath string `json:"messagingSocketPath"`
	Kind                string `json:"kind"`
}

func (s Session) Binding() adapter.Binding {
	return adapter.Binding{
		Harness:         adapter.Claude,
		SessionRef:      s.SessionID,
		HarnessEndpoint: s.MessagingSocketPath,
		HarnessPID:      s.PID,
	}
}

func (s Session) Busy() bool { return s.Status == "busy" }

type Adapter struct {
	SessionsDir string
	PPID        func() int
	// UDSPath is this shim's agent socket, sent as the envelope's reply address.
	UDSPath     string
	Alive       func(pid int) bool
	DialTimeout time.Duration
	AckTimeout  time.Duration
}

var _ adapter.Adapter = (*Adapter)(nil)

func DefaultSessionsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "sessions"), nil
}

func New(udsPath string) (*Adapter, error) {
	dir, err := DefaultSessionsDir()
	if err != nil {
		return nil, err
	}
	a := &Adapter{SessionsDir: dir, PPID: os.Getppid, UDSPath: udsPath}
	if _, err := FromAddress(udsPath); err != nil {
		return nil, err
	}
	return a, nil
}

func alive(pid int) bool {
	_, err := procstart.StartTime(pid)
	return err == nil
}

func (a *Adapter) alive(pid int) bool {
	if a.Alive != nil {
		return a.Alive(pid)
	}
	return alive(pid)
}

func (a *Adapter) readSession(pid int) (Session, error) {
	b, err := os.ReadFile(filepath.Join(a.SessionsDir, strconv.Itoa(pid)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return Session{}, fmt.Errorf("claude: session %d: %w", pid, err)
	}
	if s.PID != pid || s.SessionID == "" {
		return Session{}, fmt.Errorf("claude: session file %d.json names pid %d session %q", pid, s.PID, s.SessionID)
	}
	return s, nil
}

// BindFromParent resolves the Claude session that started this process.
func (a *Adapter) BindFromParent() (Session, error) {
	ppid := os.Getppid
	if a.PPID != nil {
		ppid = a.PPID
	}
	return a.readSession(ppid())
}

func (a *Adapter) find(sessionRef string) (Session, error) {
	paths, err := filepath.Glob(filepath.Join(a.SessionsDir, "*.json"))
	if err != nil {
		return Session{}, err
	}
	var found []Session
	for _, p := range paths {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(p), ".json"))
		if err != nil {
			continue
		}
		s, err := a.readSession(pid)
		if err != nil || s.SessionID != sessionRef || !a.alive(pid) {
			continue
		}
		found = append(found, s)
	}
	switch len(found) {
	case 0:
		return Session{}, ErrNoSession
	case 1:
		return found[0], nil
	}
	return Session{}, ErrAmbiguous
}

func (a *Adapter) Register(_ context.Context, sessionRef string) (adapter.Binding, error) {
	s, err := a.find(sessionRef)
	if err != nil {
		return adapter.Binding{}, err
	}
	return s.Binding(), nil
}

func (a *Adapter) Status(_ context.Context, sessionRef string) (adapter.SessionStatus, error) {
	s, err := a.find(sessionRef)
	if errors.Is(err, ErrNoSession) {
		return adapter.SessionStatus{}, nil
	}
	if err != nil {
		return adapter.SessionStatus{}, err
	}
	_, serr := os.Stat(s.MessagingSocketPath)
	return adapter.SessionStatus{Loaded: true, Busy: s.Busy(), Reachable: s.MessagingSocketPath != "" && serr == nil}, nil
}

func (a *Adapter) key(pid int) (string, error) {
	keys, err := filepath.Glob(filepath.Join(a.SessionsDir, strconv.Itoa(pid)+".*.key"))
	if err != nil {
		return "", err
	}
	if len(keys) != 1 {
		return "", ErrNoKey
	}
	b, err := os.ReadFile(keys[0])
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", ErrNoKey
	}
	return t, nil
}

// FromAddress returns the envelope reply address for a socket path.
func FromAddress(udsPath string) (string, error) {
	from := "uds:" + udsPath
	if !fromRE.MatchString(from) {
		return "", fmt.Errorf("%w: %q", ErrBadFrom, from)
	}
	return from, nil
}

var attrEscaper = strings.NewReplacer(`&`, "&amp;", `"`, "&quot;", `<`, "&lt;", `>`, "&gt;")

// EscapeBody makes peer text inert inside the envelope: no '<' survives, so
// the body can neither close the envelope nor open a nested one.
func EscapeBody(s string) string { return strings.ReplaceAll(s, "<", "&lt;") }

func Envelope(from, fromName, body string) string {
	return `<cross-session-message from="` + from + `" from-name="` + attrEscaper.Replace(fromName) + "\">\n" +
		EscapeBody(body) + "\n</cross-session-message>"
}

func Body(m msg.Message) string {
	texts := make([]string, 0, len(m.Parts))
	for _, p := range m.Parts {
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n")
}

type authFrame struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

type userFrame struct {
	Type    string      `json:"type"`
	Message userMessage `json:"message"`
}

type userMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ackFrame struct {
	Type  string `json:"type"`
	State string `json:"state"`
}

// Inject writes m into the session named by d.Target. Failures before the
// first byte reaches the socket are unreachable; anything after is unknown
// unless the socket returns an ack line (CQ2).
func (a *Adapter) Inject(ctx context.Context, m msg.Message, d adapter.Delivery) (adapter.State, error) {
	from, err := FromAddress(a.UDSPath)
	if err != nil {
		return adapter.Unreachable, err
	}
	s, err := a.readSession(d.Target.HarnessPID)
	if err != nil {
		return adapter.Unreachable, err
	}
	if s.SessionID != d.Target.SessionRef || (d.Target.HarnessEndpoint != "" && s.MessagingSocketPath != d.Target.HarnessEndpoint) {
		return adapter.Unreachable, ErrStaleBinding
	}
	token, err := a.key(s.PID)
	if err != nil {
		return adapter.Unreachable, err
	}
	payload, err := frames(token, Envelope(from, m.From, Body(m)))
	if err != nil {
		return adapter.Unreachable, err
	}

	dt := a.DialTimeout
	if dt == 0 {
		dt = DefaultDialTimeout
	}
	dialer := net.Dialer{Timeout: dt}
	conn, err := dialer.DialContext(ctx, "unix", s.MessagingSocketPath)
	if err != nil {
		return adapter.Unreachable, err
	}
	defer conn.Close()
	at := a.AckTimeout
	if at == 0 {
		at = DefaultAckTimeout
	}
	deadline := time.Now().Add(at)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	if n, err := conn.Write(payload); err != nil {
		if n == 0 {
			return adapter.Unreachable, err
		}
		return adapter.Unknown, err
	}
	return readAck(conn)
}

func frames(token, content string) ([]byte, error) {
	auth, err := json.Marshal(authFrame{Type: "auth", Token: token})
	if err != nil {
		return nil, err
	}
	user, err := json.Marshal(userFrame{Type: "user", Message: userMessage{Role: "user", Content: content}})
	if err != nil {
		return nil, err
	}
	out := append(auth, '\n')
	out = append(out, user...)
	return append(out, '\n'), nil
}

var ErrNoAck = errors.New("claude: socket returned no acknowledgement")

func readAck(conn net.Conn) (adapter.State, error) {
	r := bufio.NewReader(io.LimitReader(conn, maxAckLine))
	line, err := r.ReadBytes('\n')
	if len(strings.TrimSpace(string(line))) == 0 {
		if err == nil {
			err = ErrNoAck
		}
		return adapter.Unknown, fmt.Errorf("%w: %v", ErrNoAck, err)
	}
	var ack ackFrame
	if jerr := json.Unmarshal(line, &ack); jerr != nil || ack.Type != "ack" {
		return adapter.Unknown, fmt.Errorf("%w: unexpected reply %q", ErrNoAck, strings.TrimSpace(string(line)))
	}
	switch st := adapter.State(ack.State); st {
	case "":
		return adapter.Accepted, nil
	case adapter.Accepted, adapter.Queued, adapter.Rejected, adapter.Unreachable, adapter.Unknown:
		return st, nil
	}
	return adapter.Unknown, fmt.Errorf("%w: ack state %q", ErrNoAck, ack.State)
}
