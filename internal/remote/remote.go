// Package remote holds this host's identity and the files that describe its
// remote setup: the host key and certificate, remote.json and peers.json.
// Docs: docs/plans/release-0.2.0.md sections 3.3 and 6.
package remote

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	DefaultPort = 7745
	version     = 1
	maxFile     = 1 << 20
	// A file modified this recently may be rewritten again within the same
	// filesystem timestamp tick, so the cache does not trust its stat.
	racyWindow = 2 * time.Second
)

var (
	ErrNotConfigured = errors.New("remote: not configured")
	ErrMode          = errors.New("remote: file or directory has the wrong mode")
	ErrAliasTaken    = errors.New("remote: alias belongs to another peer")
)

var fingerprintRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// InterfaceAddrs lists this host's interface addresses; tests replace it.
var InterfaceAddrs = net.InterfaceAddrs

type Host struct {
	Certificate tls.Certificate
	Cert        *x509.Certificate
	Fingerprint string
}

type Config struct {
	Version    int    `json:"version"`
	Alias      string `json:"alias"`
	Bind       string `json:"bind"`
	Port       int    `json:"port"`
	UnitSHA256 string `json:"unit_sha256,omitempty"`
}

type Peer struct {
	Alias       string    `json:"alias"`
	Address     string    `json:"address"`
	Fingerprint string    `json:"fingerprint"`
	PairedAt    time.Time `json:"paired_at"`
}

// ConfigDir is ~/.config/crosstalk.
func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "crosstalk"), nil
}

// Fingerprint is the lowercase hex SHA-256 of the certificate's
// SubjectPublicKeyInfo, so it survives re-issuing a certificate for one key.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return checkDir(dir)
}

func checkDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s is %v, want a 0700 directory", ErrMode, dir, fi.Mode())
	}
	return nil
}

// readChecked opens path without following a symlink, checks the open file is
// regular with exactly mode, and reads it from that same descriptor.
func readChecked(path string, mode os.FileMode) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%w: %s is a symlink", ErrMode, path)
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != mode {
		return nil, fmt.Errorf("%w: %s is %v, want a regular file with mode %04o", ErrMode, path, fi.Mode(), mode)
	}
	return readLimited(f, path)
}

func readLimited(f *os.File, path string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxFile {
		return nil, fmt.Errorf("remote: %s is larger than %d bytes", path, maxFile)
	}
	return b, nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(data)
	if err := errors.Join(werr, tmp.Chmod(mode), tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// lock takes a blocking exclusive flock on dir/name and returns its release.
// An existing lock file must be a regular 0600 file.
func lock(dir, name string) (func(), error) {
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		f.Close()
		return nil, fmt.Errorf("%w: %s is %v, want a regular file with mode 0600", ErrMode, path, fi.Mode())
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func parseKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := readChecked(path, 0o600)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("remote: %s is not PEM", path)
	}
	key, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("remote: %s is not an ECDSA P-256 key", path)
	}
	return key, nil
}

func parseHost(dir string) (Host, error) {
	if err := checkDir(dir); err != nil {
		return Host{}, err
	}
	keyPath, crtPath := filepath.Join(dir, "host.key"), filepath.Join(dir, "host.crt")
	key, err := parseKey(keyPath)
	if err != nil {
		return Host{}, err
	}
	b, err := readChecked(crtPath, 0o644)
	if err != nil {
		return Host{}, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return Host{}, fmt.Errorf("remote: %s is not PEM", crtPath)
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return Host{}, fmt.Errorf("remote: %s: %w", crtPath, err)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return Host{}, fmt.Errorf("remote: %s does not belong to %s", crtPath, keyPath)
	}
	return Host{
		Certificate: tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key, Leaf: cert},
		Cert:        cert,
		Fingerprint: Fingerprint(cert),
	}, nil
}

// LoadHost reads the host key and certificate; it never creates them.
func LoadHost(dir string) (Host, error) {
	h, err := parseHost(dir)
	if errors.Is(err, os.ErrNotExist) {
		return Host{}, fmt.Errorf("%w: %v", ErrNotConfigured, err)
	}
	return h, err
}

// LoadOrCreateHost loads the host identity, creating the key and then the
// certificate if missing. An existing key is never replaced.
func LoadOrCreateHost(dir string) (Host, error) {
	if err := ensureDir(dir); err != nil {
		return Host{}, err
	}
	unlock, err := lock(dir, "host.lock")
	if err != nil {
		return Host{}, err
	}
	defer unlock()
	keyPath, crtPath := filepath.Join(dir, "host.key"), filepath.Join(dir, "host.crt")
	if _, err := os.Lstat(keyPath); errors.Is(err, os.ErrNotExist) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return Host{}, err
		}
		der, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return Host{}, err
		}
		if err := writeAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
			return Host{}, err
		}
	} else if err != nil {
		return Host{}, err
	}
	if _, err := os.Lstat(crtPath); errors.Is(err, os.ErrNotExist) {
		key, err := parseKey(keyPath)
		if err != nil {
			return Host{}, err
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
		if err != nil {
			return Host{}, err
		}
		now := time.Now()
		tpl := &x509.Certificate{
			SerialNumber: serial,
			Subject:      pkix.Name{CommonName: "crosstalk"},
			NotBefore:    now.Add(-time.Hour),
			NotAfter:     now.AddDate(20, 0, 0),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
		if err != nil {
			return Host{}, err
		}
		if err := writeAtomic(crtPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
			return Host{}, err
		}
	} else if err != nil {
		return Host{}, err
	}
	return parseHost(dir)
}

func ValidAlias(a string) error {
	switch {
	case a == "" || len(a) > 63:
		return errors.New("alias must be 1 to 63 bytes")
	case !utf8.ValidString(a):
		return errors.New("alias is not valid UTF-8")
	case strings.ContainsRune(a, '@'):
		return errors.New("alias may not contain @")
	}
	for _, r := range a {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("alias may not contain spaces or control characters")
		}
	}
	return nil
}

// DefaultAlias is the first label of the hostname, lowercased.
func DefaultAlias() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	if ValidAlias(h) != nil {
		return "host"
	}
	return h
}

func checkBindClass(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return a, fmt.Errorf("%q is not an IP address", s)
	}
	a = a.Unmap()
	if a.IsUnspecified() || a.IsMulticast() {
		return a, fmt.Errorf("%s is a wildcard or multicast address; give one address of this host", s)
	}
	return a, nil
}

// ValidateBind accepts one unicast IP. Unless force is set it must be
// assigned to an interface of this host.
func ValidateBind(s string, force bool) (string, error) {
	a, err := checkBindClass(s)
	if err != nil {
		return "", err
	}
	if force {
		return a.String(), nil
	}
	addrs, err := InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, ia := range addrs {
		if p, err := netip.ParsePrefix(ia.String()); err == nil && p.Addr().Unmap() == a {
			return a.String(), nil
		}
	}
	return "", fmt.Errorf("%s is not an address of any interface on this host (--force allows it)", s)
}

func validateConfig(c Config) error {
	if c.Version != version {
		return fmt.Errorf("version %d, want %d", c.Version, version)
	}
	if err := ValidAlias(c.Alias); err != nil {
		return err
	}
	if _, err := checkBindClass(c.Bind); err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d outside 1-65535", c.Port)
	}
	return nil
}

func LoadConfig(dir string) (Config, error) {
	path := filepath.Join(dir, "remote.json")
	if err := checkDir(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, ErrNotConfigured
		}
		return Config{}, err
	}
	b, err := readChecked(path, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, ErrNotConfigured
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("remote: %s: %w", path, err)
	}
	if err := validateConfig(c); err != nil {
		return Config{}, fmt.Errorf("remote: %s: %w", path, err)
	}
	return c, nil
}

// UpdateConfig reads remote.json under remote.lock, passes it to fn, and
// writes the result atomically before releasing the lock.
func UpdateConfig(dir string, fn func(old Config, configured bool) (Config, error)) (Config, error) {
	if err := ensureDir(dir); err != nil {
		return Config{}, err
	}
	unlock, err := lock(dir, "remote.lock")
	if err != nil {
		return Config{}, err
	}
	defer unlock()
	old, err := LoadConfig(dir)
	configured := err == nil
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return Config{}, err
	}
	c, err := fn(old, configured)
	if err != nil {
		return Config{}, err
	}
	c.Version = version
	if err := validateConfig(c); err != nil {
		return Config{}, err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return Config{}, err
	}
	return c, writeAtomic(filepath.Join(dir, "remote.json"), append(b, '\n'), 0o600)
}

func SaveConfig(dir string, c Config) error {
	_, err := UpdateConfig(dir, func(Config, bool) (Config, error) { return c, nil })
	return err
}

type peersFile struct {
	Version int    `json:"version"`
	Peers   []Peer `json:"peers"`
}

type statKey struct {
	mtime time.Time
	size  int64
	ino   uint64
}

// Peers is the paired-host table, re-read whenever the file changes.
type Peers struct {
	dir    string
	mu     sync.Mutex
	key    statKey
	cached []Peer
	valid  bool
}

func NewPeers(dir string) *Peers { return &Peers{dir: dir} }

func (p *Peers) path() string { return filepath.Join(p.dir, "peers.json") }

func (p *Peers) parse(b []byte) ([]Peer, error) {
	var f peersFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("remote: %s: %w", p.path(), err)
	}
	if f.Version != version {
		return nil, fmt.Errorf("remote: %s: version %d, want %d", p.path(), f.Version, version)
	}
	seenFP, seenAlias := map[string]bool{}, map[string]bool{}
	for _, q := range f.Peers {
		if err := validatePeer(q); err != nil {
			return nil, fmt.Errorf("remote: %s: %w", p.path(), err)
		}
		if seenFP[q.Fingerprint] || seenAlias[q.Alias] {
			return nil, fmt.Errorf("remote: %s: duplicate peer %q", p.path(), q.Alias)
		}
		seenFP[q.Fingerprint], seenAlias[q.Alias] = true, true
	}
	return f.Peers, nil
}

func validatePeer(q Peer) error {
	if err := ValidAlias(q.Alias); err != nil {
		return fmt.Errorf("peer %q: %w", q.Alias, err)
	}
	if !fingerprintRE.MatchString(q.Fingerprint) {
		return fmt.Errorf("peer %q: fingerprint is not 64 lowercase hex digits", q.Alias)
	}
	if q.Address == "" {
		return fmt.Errorf("peer %q: empty address", q.Alias)
	}
	return nil
}

func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

// read returns the parsed table; a missing file is an empty table. It checks
// the mode on every call and reparses only when the file changed.
func (p *Peers) read() ([]Peer, error) {
	if err := checkDir(p.dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	f, err := os.OpenFile(p.path(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		p.valid = false
		return nil, nil
	}
	if err != nil {
		p.valid = false
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		p.valid = false
		return nil, fmt.Errorf("%w: %s is %v, want a regular file with mode 0600", ErrMode, p.path(), fi.Mode())
	}
	k := statKey{fi.ModTime(), fi.Size(), inode(fi)}
	if p.valid && p.key == k && time.Since(k.mtime) > racyWindow {
		return p.cached, nil
	}
	b, err := readLimited(f, p.path())
	if err != nil {
		return nil, err
	}
	ps, err := p.parse(b)
	if err != nil {
		p.valid = false
		return nil, err
	}
	p.key, p.cached, p.valid = k, ps, true
	return ps, nil
}

func (p *Peers) List() ([]Peer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ps, err := p.read()
	if err != nil {
		return nil, err
	}
	return append([]Peer(nil), ps...), nil
}

// modify runs fn on the current table under peers.lock and writes the result
// atomically. A table that does not parse is never overwritten.
func (p *Peers) modify(fn func([]Peer) ([]Peer, error)) error {
	if err := ensureDir(p.dir); err != nil {
		return err
	}
	unlock, err := lock(p.dir, "peers.lock")
	if err != nil {
		return err
	}
	defer unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.valid = false
	cur, err := p.read()
	if err != nil {
		return err
	}
	next, err := fn(append([]Peer(nil), cur...))
	if err != nil {
		return err
	}
	if next == nil {
		next = []Peer{}
	}
	b, err := json.MarshalIndent(peersFile{Version: version, Peers: next}, "", "  ")
	if err != nil {
		return err
	}
	p.valid = false
	return writeAtomic(p.path(), append(b, '\n'), 0o600)
}

// Upsert adds the peer, or updates the entry with the same fingerprint.
func (p *Peers) Upsert(q Peer) error {
	if err := validatePeer(q); err != nil {
		return err
	}
	return p.modify(func(ps []Peer) ([]Peer, error) {
		at := -1
		for i, o := range ps {
			if o.Fingerprint == q.Fingerprint {
				at = i
			} else if o.Alias == q.Alias {
				return nil, fmt.Errorf("%w: %q", ErrAliasTaken, q.Alias)
			}
		}
		if at >= 0 {
			if q.PairedAt.IsZero() {
				q.PairedAt = ps[at].PairedAt
			}
			ps[at] = q
			return ps, nil
		}
		if q.PairedAt.IsZero() {
			q.PairedAt = time.Now().UTC()
		}
		return append(ps, q), nil
	})
}

func (p *Peers) Remove(alias string) error {
	return p.modify(func(ps []Peer) ([]Peer, error) {
		for i, o := range ps {
			if o.Alias == alias {
				return append(ps[:i], ps[i+1:]...), nil
			}
		}
		return nil, fmt.Errorf("remote: no peer %q", alias)
	})
}

// Lookup finds the peer with this fingerprint in the current table.
func (p *Peers) Lookup(fingerprint string) (Peer, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ps, err := p.read()
	if err != nil {
		return Peer{}, false, err
	}
	for _, q := range ps {
		if q.Fingerprint == fingerprint {
			return q, true, nil
		}
	}
	return Peer{}, false, nil
}
