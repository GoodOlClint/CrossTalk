package remote

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "crosstalk")
}

func TestFingerprintIsSPKISHA256(t *testing.T) {
	dir := newDir(t)
	h, err := LoadOrCreateHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(h.Cert.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(spki)
	want := hex.EncodeToString(sum[:])
	if h.Fingerprint != want {
		t.Fatalf("fingerprint %s, want SHA-256 of the SPKI %s", h.Fingerprint, want)
	}
	if rawSum := sha256.Sum256(h.Cert.Raw); h.Fingerprint == hex.EncodeToString(rawSum[:]) {
		t.Fatal("fingerprint is the hash of the whole certificate, not the SPKI")
	}
	if got := Fingerprint(h.Cert); got != want {
		t.Fatalf("Fingerprint() %s, want %s", got, want)
	}
	h2, err := LoadHost(dir)
	if err != nil || h2.Fingerprint != want {
		t.Fatalf("reload: %v fp %s, want %s", err, h2.Fingerprint, want)
	}
	if err := os.Remove(filepath.Join(dir, "host.crt")); err != nil {
		t.Fatal(err)
	}
	h3, err := LoadOrCreateHost(dir)
	if err != nil || h3.Fingerprint != want {
		t.Fatalf("certificate regenerated from the same key changed the fingerprint: %v %s", err, h3.Fingerprint)
	}
	if time.Until(h.Cert.NotAfter) < 19*365*24*time.Hour {
		t.Fatalf("certificate expires %v, want about 20 years", h.Cert.NotAfter)
	}
}

func TestPeersHandEditReloaded(t *testing.T) {
	dir := newDir(t)
	p := NewPeers(dir)
	fp := strings.Repeat("a", 64)
	if err := p.Upsert(Peer{Alias: "desk", Address: "10.0.0.5:7745", Fingerprint: fp, PairedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	ps, err := p.List()
	if err != nil || len(ps) != 1 || ps[0].Address != "10.0.0.5:7745" {
		t.Fatalf("List = %v, %v", ps, err)
	}
	path := filepath.Join(dir, "peers.json")
	b, _ := os.ReadFile(path)
	edited := strings.Replace(string(b), "10.0.0.5:7745", "10.0.0.6:7745", 1)
	if len(edited) != len(b) {
		t.Fatal("test edit must keep the size")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	ps, err = p.List()
	if err != nil || len(ps) != 1 || ps[0].Address != "10.0.0.6:7745" {
		t.Fatalf("same-size in-place edit not seen: %v, %v", ps, err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = p.List()
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("unparseable file: err %v, want one naming %s", err, path)
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if ps, err = p.List(); err != nil || len(ps) != 1 {
		t.Fatalf("repaired file not read: %v, %v", ps, err)
	}
	if err := os.WriteFile(path, []byte(`{"version": 2, "peers": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = p.List(); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("unknown version: err %v, want one naming %s", err, path)
	}
}

func TestConfigRefusesWrongMode(t *testing.T) {
	dir := newDir(t)
	if _, err := LoadOrCreateHost(dir); err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(dir, Config{Version: 1, Alias: "a", Bind: "127.0.0.1", Port: 7745}); err != nil {
		t.Fatal(err)
	}
	if err := NewPeers(dir).Upsert(Peer{Alias: "b", Address: "h:1", Fingerprint: strings.Repeat("b", 64)}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		file string
		bad  os.FileMode
		load func() error
	}{
		{"host.key", 0o644, func() error { _, err := LoadHost(dir); return err }},
		{"host.key", 0o640, func() error { _, err := LoadOrCreateHost(dir); return err }},
		{"host.crt", 0o666, func() error { _, err := LoadHost(dir); return err }},
		{"remote.json", 0o644, func() error { _, err := LoadConfig(dir); return err }},
		{"peers.json", 0o644, func() error { _, err := NewPeers(dir).List(); return err }},
	}
	for _, c := range cases {
		path := filepath.Join(dir, c.file)
		fi, _ := os.Stat(path)
		good := fi.Mode().Perm()
		if err := os.Chmod(path, c.bad); err != nil {
			t.Fatal(err)
		}
		if err := c.load(); !errors.Is(err, ErrMode) {
			t.Errorf("%s mode %o: err %v, want ErrMode", c.file, c.bad, err)
		}
		os.Chmod(path, good)
		if err := c.load(); err != nil {
			t.Errorf("%s restored to %o: %v", c.file, good, err)
		}
	}
	key := filepath.Join(dir, "host.key")
	real := key + ".real"
	os.Rename(key, real)
	os.Symlink(real, key)
	if _, err := LoadHost(dir); !errors.Is(err, ErrMode) {
		t.Errorf("symlinked key: err %v, want ErrMode", err)
	}
	os.Remove(key)
	os.Rename(real, key)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHost(dir); !errors.Is(err, ErrMode) {
		t.Errorf("config dir 0755: err %v, want ErrMode", err)
	}
}

func TestPeersAtomicUpdateByFingerprint(t *testing.T) {
	dir := newDir(t)
	p := NewPeers(dir)
	fp := strings.Repeat("c", 64)
	for _, addr := range []string{"10.0.0.1:7745", "10.0.0.2:7745"} {
		if err := p.Upsert(Peer{Alias: "desk", Address: addr, Fingerprint: fp}); err != nil {
			t.Fatal(err)
		}
	}
	ps, err := p.List()
	if err != nil || len(ps) != 1 || ps[0].Address != "10.0.0.2:7745" {
		t.Fatalf("re-adding a fingerprint: %v, %v; want one entry with the new address", ps, err)
	}
	if err := p.Upsert(Peer{Alias: "desk", Address: "x:1", Fingerprint: strings.Repeat("d", 64)}); !errors.Is(err, ErrAliasTaken) {
		t.Fatalf("alias held by another fingerprint: err %v, want ErrAliasTaken", err)
	}
	if err := p.Upsert(Peer{Alias: "laptop", Address: "10.0.0.2:7745", Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	if ps, _ = p.List(); len(ps) != 1 || ps[0].Alias != "laptop" {
		t.Fatalf("alias change by fingerprint: %v", ps)
	}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := NewPeers(dir)
			if err := q.Upsert(Peer{Alias: fmt.Sprintf("h%d", i), Address: "a:1", Fingerprint: fmt.Sprintf("%064x", i+1)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ps, err = p.List(); err != nil || len(ps) != 17 {
		t.Fatalf("after 16 concurrent upserts: %d peers, %v; want 17", len(ps), err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	fi, _ := os.Stat(filepath.Join(dir, "peers.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("peers.json mode %o, want 0600", fi.Mode().Perm())
	}
	if err := p.Remove("laptop"); err != nil {
		t.Fatal(err)
	}
	if ps, _ = p.List(); len(ps) != 16 {
		t.Fatalf("after Remove: %d peers", len(ps))
	}
	if err := p.Remove("nobody"); err == nil {
		t.Fatal("Remove of an unknown alias succeeded")
	}
}
