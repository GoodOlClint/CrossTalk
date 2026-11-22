package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/harness"
)

// HandoffFormat is the refresh_handoff version this binary writes and reads
// (ADR 0030).
const HandoffFormat = 1

// HandoffFDEnv names the inherited descriptor of the handoff file.
const HandoffFDEnv = "CROSSTALK_REFRESH_STATE_FD"

const maxHandoff = 16 << 20

// BinaryIdentity is the device and inode of the binary path the shim runs.
type BinaryIdentity struct {
	Path string `json:"path"`
	Dev  uint64 `json:"dev"`
	Ino  uint64 `json:"ino"`
}

// IdentityOf stats path, following a symlink.
func IdentityOf(path string) (BinaryIdentity, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return BinaryIdentity{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return BinaryIdentity{}, errors.New("mcpserver: no device and inode for " + path)
	}
	return BinaryIdentity{Path: path, Dev: uint64(st.Dev), Ino: uint64(st.Ino)}, nil
}

// AgentHandoff is one bound agent. Binding and BoundAt come from the Backend
// (Refresher); the rest from the mcpserver Agent.
type AgentHandoff struct {
	ID            string                 `json:"id"`
	Harness       string                 `json:"harness"`
	SessionRef    string                 `json:"session_ref"`
	Cwd           string                 `json:"cwd,omitempty"`
	Req           harness.RequestContext `json:"req"`
	HarnessName   string                 `json:"harness_name,omitempty"`
	ViaCall       bool                   `json:"via_call,omitempty"`
	PendingNotice []string               `json:"pending_notice,omitempty"`
	Binding       adapter.Binding        `json:"binding"`
	BoundAt       time.Time              `json:"bound_at"`
}

// Handoff is the state a shim hands to its own next image. The journals, the
// claims' locks and the bearer tokens stay on disk and are named by id only.
type Handoff struct {
	RefreshHandoff int                        `json:"refresh_handoff"`
	Version        string                     `json:"version"`
	Session        SessionState               `json:"session"`
	Agents         []AgentHandoff             `json:"agents"`
	Harness        map[string]json.RawMessage `json:"harness,omitempty"`
	Binary         BinaryIdentity             `json:"binary"`
}

// Refresher is implemented by a Backend that holds per-agent state to carry.
type Refresher interface {
	// RefreshExport fills Binding and BoundAt of a's handoff.
	RefreshExport(a *Agent, h *AgentHandoff)
	// RefreshResume applies the carried values to the re-registered a.
	RefreshResume(a *Agent, h AgentHandoff)
}

// Handoff snapshots the ready agents, the harnesses' refresh state and st.
// The caller has quiesced the connection.
func (s *Server) Handoff(st SessionState, bin BinaryIdentity) *Handoff {
	h := &Handoff{RefreshHandoff: HandoffFormat, Version: Version, Session: st, Binary: bin, Agents: []AgentHandoff{}}
	s.mu.Lock()
	agents := make([]*Agent, 0, len(s.agents))
	for _, a := range s.agents {
		if a.isReady() {
			agents = append(agents, a)
		}
	}
	via := make(map[*Agent]bool, len(agents))
	for _, a := range agents {
		via[a] = a.viaCall
	}
	s.mu.Unlock()
	slices.SortFunc(agents, func(a, b *Agent) int { return strings.Compare(a.id, b.id) })
	r, _ := s.backend.(Refresher)
	for _, a := range agents {
		a.mu.Lock()
		ah := AgentHandoff{ID: a.id, Harness: string(a.harness), SessionRef: a.sessionRef, Cwd: a.Descriptor().Cwd, Req: a.req,
			HarnessName: a.harnessName, ViaCall: via[a], PendingNotice: slices.Clone(a.pendingNotice)}
		a.mu.Unlock()
		if r != nil {
			r.RefreshExport(a, &ah)
		}
		h.Agents = append(h.Agents, ah)
	}
	for _, hh := range s.harnesses {
		if rf, ok := hh.(harness.Refreshable); ok {
			if raw := rf.RefreshState(); len(raw) > 0 {
				if h.Harness == nil {
					h.Harness = map[string]json.RawMessage{}
				}
				h.Harness[string(hh.Name())] = raw
			}
		}
	}
	return h
}

// WriteHandoff writes h to an unlinked 0600 temp file and returns it
// positioned at the start, for a child to inherit.
func WriteHandoff(h *Handoff) (*os.File, error) {
	b, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if len(b) > maxHandoff {
		return nil, fmt.Errorf("refresh handoff: %d bytes exceeds the %d byte limit the reader accepts", len(b), maxHandoff)
	}
	f, err := os.CreateTemp("", "crosstalk-refresh-")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ReadHandoff parses a handoff. It rejects an unknown refresh_handoff, a
// truncated or malformed document, and an agent without an id.
func ReadHandoff(r io.Reader) (*Handoff, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxHandoff+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxHandoff {
		return nil, errors.New("refresh handoff too large")
	}
	var h Handoff
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("refresh handoff: %w", err)
	}
	if h.RefreshHandoff != HandoffFormat {
		return nil, fmt.Errorf("refresh handoff: unknown version %d", h.RefreshHandoff)
	}
	if _, err := h.Session.serverState(); err != nil {
		return nil, fmt.Errorf("refresh handoff: session state: %w", err)
	}
	for _, a := range h.Agents {
		if a.ID == "" || a.Harness == "" || a.SessionRef == "" {
			return nil, errors.New("refresh handoff: agent without id, harness or session_ref")
		}
	}
	return &h, nil
}

// HandoffFromEnv reads and closes the descriptor HandoffFDEnv names and
// unsets the variable so no child inherits it. It returns nil, nil when the
// variable is unset.
func HandoffFromEnv() (*Handoff, error) {
	v, ok := os.LookupEnv(HandoffFDEnv)
	if !ok {
		return nil, nil
	}
	os.Unsetenv(HandoffFDEnv)
	fd, err := strconv.Atoi(v)
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("refresh handoff: bad %s %q", HandoffFDEnv, v)
	}
	f := os.NewFile(uintptr(fd), "refresh-handoff")
	if f == nil {
		return nil, fmt.Errorf("refresh handoff: bad descriptor %d", fd)
	}
	defer f.Close()
	if err := checkHandoffFile(f); err != nil {
		return nil, err
	}
	return ReadHandoff(f)
}

// checkHandoffFile requires what WriteHandoff makes: a regular file with no
// name left and no access for group or others, so a stale descriptor number
// (a pipe, a socket, a file of another purpose) is never read or closed as one.
func checkHandoffFile(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("refresh handoff: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 || !ok || st.Nlink != 0 {
		return errors.New("refresh handoff: descriptor is not a private unlinked regular file")
	}
	return nil
}

// serverState is what the SDK needs to accept calls with no initialize.
func (st SessionState) serverState() (*mcp.ServerSessionState, error) {
	if len(st.InitializeParams) == 0 {
		return nil, nil
	}
	var ip mcp.InitializeParams
	if err := json.Unmarshal(st.InitializeParams, &ip); err != nil {
		return nil, err
	}
	ss := &mcp.ServerSessionState{InitializeParams: &ip, NegotiatedProtocolVersion: st.ProtocolVersion}
	if st.Initialized {
		ss.InitializedParams = &mcp.InitializedParams{}
	}
	if len(st.SetLevelParams) > 0 {
		var sl mcp.SetLoggingLevelParams
		if err := json.Unmarshal(st.SetLevelParams, &sl); err != nil {
			return nil, err
		}
		ss.LogLevel = sl.Level
	}
	return ss, nil
}
