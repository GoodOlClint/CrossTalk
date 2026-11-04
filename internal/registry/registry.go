package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/procstart"
)

var (
	ErrAgentHeld = errors.New("registry: agent record lock held by a live registration")
	ErrNameHeld  = errors.New("registry: name lock held by a live registration")
	ErrClosed    = errors.New("registry: registration closed")
)

type Registry struct {
	Dir       string
	StartTime func(pid int) (time.Time, error)
	// Superseded reports that d's session ended although d's shim is still
	// running (harness.Superseded); nil means no descriptor is. Set it before
	// the registry is shared.
	Superseded func(d Descriptor) bool
}

func New(dir string) *Registry {
	return &Registry{Dir: dir, StartTime: procstart.StartTime}
}

func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "crosstalk"), nil
}

func (r *Registry) agentsDir() string { return filepath.Join(r.Dir, "agents") }
func (r *Registry) namesDir() string  { return filepath.Join(r.Dir, "names") }

func (r *Registry) DescriptorPath(agentID string) string {
	return filepath.Join(r.agentsDir(), agentID+".json")
}

func (r *Registry) agentLockPath(agentID string) string {
	return filepath.Join(r.agentsDir(), agentID+".lock")
}

func (r *Registry) nameLockPath(name string) string {
	esc := url.PathEscape(name)
	if strings.HasPrefix(esc, ".") {
		esc = "%2E" + esc[1:]
	}
	return filepath.Join(r.namesDir(), esc+".lock")
}

func (r *Registry) UDSPath(agentID string) string {
	return filepath.Join(r.Dir, "sock", agentID+".sock")
}

// tryLock takes an exclusive flock without blocking. Lock files are never
// deleted: unlinking one would let a later opener lock a different inode
// while the old holder still believes it owns the name.
func tryLock(path string) (*os.File, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

type Registration struct {
	reg *Registry

	mu        sync.Mutex
	agentLock *os.File
	nameLock  *os.File
	desc      Descriptor
	published bool
}

type Request struct {
	AgentID     string
	HarnessName string
	Harness     string
	Cwd         string
}

// Acquire takes the agent record lock for req.AgentID, then the alias lock
// for the harness name if free or else the first free fallback name (§3).
// Both locks are held until Close.
func (r *Registry) Acquire(req Request) (*Registration, error) {
	al, ok, err := tryLock(r.agentLockPath(req.AgentID))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrAgentHeld
	}
	var nl *os.File
	var lockErr error
	taken := func(name string) bool {
		if lockErr != nil {
			return false
		}
		f, ok, err := tryLock(r.nameLockPath(name))
		if err != nil {
			lockErr = err
			return false
		}
		if ok {
			nl = f
		}
		return !ok
	}
	name := identity.ChooseName(req.HarnessName, req.Harness, req.Cwd, req.AgentID, taken)
	if lockErr != nil {
		al.Close()
		return nil, lockErr
	}
	return &Registration{
		reg:       r,
		agentLock: al,
		nameLock:  nl,
		desc: Descriptor{
			AgentID:    req.AgentID,
			Name:       name,
			Harness:    req.Harness,
			Role:       RolePrimary,
			Cwd:        req.Cwd,
			InstanceID: msg.NewUUIDv7(),
			UDSPath:    r.UDSPath(req.AgentID),
			Installed:  true,
			Protocol:   Protocol,
		},
	}, nil
}

func (g *Registration) Name() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.desc.Name
}

func (g *Registration) InstanceID() string { return g.desc.InstanceID }

func (g *Registration) Descriptor() Descriptor {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.desc
}

// Publish fills the caller-owned fields from d, sets the identity fields this
// registration owns (agent_id, name, instance_id, pid, started_at, protocol),
// and writes the descriptor atomically. It must be called only after the
// listeners are bound and the harness binding is confirmed (§3 order).
func (g *Registration) Publish(d Descriptor) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.agentLock == nil {
		return ErrClosed
	}
	pid := os.Getpid()
	st, err := g.reg.StartTime(pid)
	if err != nil {
		return fmt.Errorf("registry: own start time: %w", err)
	}
	d.AgentID, d.Name, d.InstanceID = g.desc.AgentID, g.desc.Name, g.desc.InstanceID
	d.PID, d.StartedAt, d.Protocol, d.Installed = pid, st, Protocol, true
	if d.Role == "" {
		d.Role = RolePrimary
	}
	if d.UDSPath == "" {
		d.UDSPath = g.desc.UDSPath
	}
	if d.Harness == "" {
		d.Harness = g.desc.Harness
	}
	if d.Cwd == "" {
		d.Cwd = g.desc.Cwd
	}
	if err := writeAtomic(g.reg.DescriptorPath(d.AgentID), d); err != nil {
		return err
	}
	g.desc, g.published = d, true
	return nil
}

// ReleaseName removes the descriptor and releases the alias lock but keeps the
// agent lock, for a superseded agent whose handlers did not stop: the agent
// stops holding its name while its journal may still be written.
func (g *Registration) ReleaseName() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.agentLock == nil {
		return nil
	}
	err := removeIfInstance(g.reg.DescriptorPath(g.desc.AgentID), g.desc.InstanceID)
	if g.nameLock != nil {
		g.nameLock.Close()
		g.nameLock = nil
	}
	return err
}

// Rename moves the alias to newName: the new alias lock is taken first, the
// descriptor is rewritten, then the old alias lock is released. agent_id,
// instance_id and every agent_id-keyed path are unchanged (CQ9).
func (g *Registration) Rename(newName string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.agentLock == nil {
		return ErrClosed
	}
	if newName == g.desc.Name {
		return nil
	}
	nl, ok, err := tryLock(g.reg.nameLockPath(newName))
	if err != nil {
		return err
	}
	if !ok {
		return ErrNameHeld
	}
	d := g.desc
	d.Name = newName
	if g.published {
		if err := writeAtomic(g.reg.DescriptorPath(d.AgentID), d); err != nil {
			nl.Close()
			return err
		}
	}
	old := g.nameLock
	g.desc, g.nameLock = d, nl
	if old != nil {
		old.Close()
	}
	return nil
}

// Close removes this registration's descriptor if it still carries this
// instance_id, then releases both locks.
func (g *Registration) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.agentLock == nil {
		return nil
	}
	err := removeIfInstance(g.reg.DescriptorPath(g.desc.AgentID), g.desc.InstanceID)
	if g.nameLock != nil {
		g.nameLock.Close()
		g.nameLock = nil
	}
	g.agentLock.Close()
	g.agentLock = nil
	return err
}

// RemoveIfOwned is the exit path for a process that may no longer hold its
// agent lock: it removes the descriptor only if it can take the agent lock and
// the file's instance_id is instanceID. It reports whether it removed the file.
func (r *Registry) RemoveIfOwned(agentID, instanceID string) (bool, error) {
	l, ok, err := tryLock(r.agentLockPath(agentID))
	if err != nil || !ok {
		return false, err
	}
	defer l.Close()
	path := r.DescriptorPath(agentID)
	d, err := readDescriptor(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if d.InstanceID != instanceID {
		return false, nil
	}
	return true, os.Remove(path)
}

func removeIfInstance(path, instanceID string) error {
	d, err := readDescriptor(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if d.InstanceID != instanceID {
		return nil
	}
	return os.Remove(path)
}

func readDescriptor(path string) (Descriptor, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Descriptor{}, err
	}
	var d Descriptor
	if err := json.Unmarshal(b, &d); err != nil {
		return Descriptor{}, fmt.Errorf("registry: %s: %w", path, err)
	}
	return d, nil
}

func writeAtomic(path string, d Descriptor) error {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(append(b, '\n'))
	if err := errors.Join(werr, tmp.Chmod(0o600), tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Live reports whether d's pid is alive and is the same process that wrote d.
func (r *Registry) Live(d Descriptor) bool {
	st, err := r.StartTime(d.PID)
	return err == nil && st.Equal(d.StartedAt)
}

// IsSuperseded reports whether d's session has ended under a shim still running.
func (r *Registry) IsSuperseded(d Descriptor) bool {
	return r.Superseded != nil && r.Superseded(d)
}

func (r *Registry) descriptors() ([]Descriptor, error) {
	paths, err := filepath.Glob(filepath.Join(r.agentsDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Descriptor
	for _, p := range paths {
		d, err := readDescriptor(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// List returns the descriptors of live sessions. A stale descriptor (dead pid,
// or a live pid whose start time differs from started_at) is removed when its
// agent lock is free, and skipped either way. A superseded descriptor is
// skipped and left for its own shim to remove.
func (r *Registry) List() ([]Descriptor, error) {
	ds, err := r.descriptors()
	if err != nil {
		return nil, err
	}
	var out []Descriptor
	for _, d := range ds {
		switch {
		case !r.Live(d):
			r.prune(d)
		case !r.IsSuperseded(d):
			out = append(out, d)
		}
	}
	return out, nil
}

// HeldBySuperseded reports whether name's holder is a superseded descriptor:
// its alias lock is still held, by a shim that has not yet released it.
func (r *Registry) HeldBySuperseded(name string) bool {
	ds, err := r.descriptors()
	if err != nil {
		return false
	}
	for _, d := range ds {
		if d.Name == name && r.Live(d) && r.IsSuperseded(d) {
			return true
		}
	}
	return false
}

func (r *Registry) prune(stale Descriptor) {
	l, ok, err := tryLock(r.agentLockPath(stale.AgentID))
	if err != nil || !ok {
		return
	}
	defer l.Close()
	path := r.DescriptorPath(stale.AgentID)
	d, err := readDescriptor(path)
	if err != nil || d.InstanceID != stale.InstanceID || r.Live(d) {
		return
	}
	os.Remove(path)
}

// Lookup resolves a name alias to a live descriptor at call time. An empty
// name matches nothing.
func (r *Registry) Lookup(name string) (Descriptor, bool, error) {
	if name == "" {
		return Descriptor{}, false, nil
	}
	ds, err := r.List()
	if err != nil {
		return Descriptor{}, false, err
	}
	for _, d := range ds {
		if d.Name == name {
			return d, true, nil
		}
	}
	return Descriptor{}, false, nil
}
