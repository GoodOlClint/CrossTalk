// Package pairing is throwaway spike code (docs/spikes/14-remote-pairing.md):
// plan section 7 steps 1 and 5, and section 3.2's pin rules.
package pairing

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	ErrUnknown = errors.New("unknown pairing id")
	ErrExpired = errors.New("pairing expired")
	ErrBadMAC  = errors.New("bad proof")
)

type Pending struct {
	ID      string    `json:"id"`
	Secret  string    `json:"secret"`
	Expires time.Time `json:"expires"`
}

type Peer struct {
	FP      string `json:"fp"`
	Alias   string `json:"alias"`
	Address string `json:"address"`
}

type Store struct {
	Dir      string
	ServerFP string
	Now      func() time.Time
}

func (s *Store) path(n string) string { return filepath.Join(s.Dir, n) }

func MAC(secret []byte, clientFP, serverFP string) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte("crosstalk-pair-v1" + clientFP + serverFP))
	return h.Sum(nil)
}

// Writes counts writeJSON calls, so a test can show which redemption wrote.
var Writes atomic.Int64

func writeJSON(path string, v any) error {
	Writes.Add(1)
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// lock takes pending.lock exclusively (flock: contends across file descriptors and processes).
func (s *Store) lock() (func(), error) {
	f, err := os.OpenFile(s.path("pending.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// Start is `remote pair`: a 32-byte secret, valid ttl from now.
func (s *Store) Start(ttl time.Duration) (id string, secret []byte, err error) {
	unlock, err := s.lock()
	if err != nil {
		return "", nil, err
	}
	defer unlock()
	secret = make([]byte, 32)
	idb := make([]byte, 8)
	rand.Read(secret)
	rand.Read(idb)
	id = hex.EncodeToString(idb)
	var p []Pending
	if err := readJSON(s.path("pending.json"), &p); err != nil {
		return "", nil, err
	}
	p = append(p, Pending{ID: id, Secret: hex.EncodeToString(secret), Expires: s.Now().Add(ttl)})
	return id, secret, writeJSON(s.path("pending.json"), p)
}

// Redeem is POST /v1/pair: clientFP comes from the handshake, never the body.
func (s *Store) Redeem(id string, mac []byte, clientFP, alias, addr string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	var p []Pending
	if err := readJSON(s.path("pending.json"), &p); err != nil {
		return err
	}
	i := -1
	for k := range p {
		if p[k].ID == id {
			i = k
		}
	}
	if i < 0 {
		return ErrUnknown
	}
	if s.Now().After(p[i].Expires) {
		return ErrExpired
	}
	secret, err := hex.DecodeString(p[i].Secret)
	if err != nil {
		return err
	}
	if !hmac.Equal(mac, MAC(secret, clientFP, s.ServerFP)) {
		return ErrBadMAC
	}
	p = append(p[:i], p[i+1:]...)
	if err := writeJSON(s.path("pending.json"), p); err != nil {
		return err
	}
	var peers []Peer
	if err := readJSON(s.path("peers.json"), &peers); err != nil {
		return err
	}
	peers = append(peers, Peer{FP: clientFP, Alias: alias, Address: addr})
	return writeJSON(s.path("peers.json"), peers)
}

func (s *Store) Peers() []Peer {
	var peers []Peer
	readJSON(s.path("peers.json"), &peers)
	return peers
}

func (s *Store) Pinned(fp string) bool {
	for _, p := range s.Peers() {
		if p.FP == fp {
			return true
		}
	}
	return false
}

// Unpair removes fp from peers.json.
func (s *Store) Unpair(fp string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	var keep []Peer
	for _, p := range s.Peers() {
		if p.FP != fp {
			keep = append(keep, p)
		}
	}
	return writeJSON(s.path("peers.json"), keep)
}
