package remote

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"time"
)

var ErrNotImplemented = errors.New("remote: not implemented")
var ErrNotConfigured = errors.New("remote: not configured")
var ErrMode = errors.New("remote: wrong file mode")
var ErrAliasTaken = errors.New("remote: alias taken")

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

type Peers struct{ dir string }

func Fingerprint(c *x509.Certificate) string    { return "" }
func LoadOrCreateHost(dir string) (Host, error) { return Host{}, ErrNotImplemented }
func LoadHost(dir string) (Host, error)         { return Host{}, ErrNotImplemented }
func LoadConfig(dir string) (Config, error)     { return Config{}, ErrNotImplemented }
func SaveConfig(dir string, c Config) error     { return ErrNotImplemented }
func NewPeers(dir string) *Peers                { return &Peers{dir: dir} }
func (p *Peers) List() ([]Peer, error)          { return nil, ErrNotImplemented }
func (p *Peers) Upsert(Peer) error              { return ErrNotImplemented }
func (p *Peers) Remove(alias string) error      { return ErrNotImplemented }
func ConfigDir() (string, error)                { return "", ErrNotImplemented }
