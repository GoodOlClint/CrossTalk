package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/remote"
	"golang.org/x/sys/unix"
)

func statusHome(t *testing.T, configured bool) (string, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	dir := filepath.Join(home, ".config", "crosstalk")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	if configured {
		if _, err := remote.LoadOrCreateHost(dir); err != nil {
			t.Fatal(err)
		}
		l := statusListener(t)
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		if err := remote.SaveConfig(dir, remote.Config{Alias: "desk", Bind: "127.0.0.1", Port: port}); err != nil {
			t.Fatal(err)
		}
	}
	return home, dir, state
}

func statusListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func statusLock(t *testing.T, state string) {
	t.Helper()
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(state, "gateway.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

func statusServing(t *testing.T, dir, state string) {
	t.Helper()
	statusLock(t, state)
	l := statusListener(t)
	c, err := remote.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Port = l.Addr().(*net.TCPAddr).Port
	if err := remote.SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	host, err := remote.LoadHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	tl := tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{host.Certificate}, MinVersion: tls.VersionTLS13})
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() {
				c.(*tls.Conn).Handshake()
				c.Close()
			}()
		}
	}()
}

func statusUnit(t *testing.T, home string) string {
	t.Helper()
	var p string
	switch runtime.GOOS {
	case "darwin":
		p = filepath.Join(home, "Library", "LaunchAgents", "dev.getcrosstalk.remote.plist")
	case "linux":
		p = filepath.Join(home, ".config", "systemd", "user", "crosstalk-remote.service")
	default:
		t.Fatal("unsupported test platform")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	unit := unitContent(runtime.GOOS, filepath.Join(home, "bin", "crosstalk"), home)
	if err := os.WriteFile(p, unit, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setUnitHash(filepath.Join(home, ".config", "crosstalk"), sha256Hex(unit)); err != nil {
		t.Fatal(err)
	}
	return p
}

func statusChecks(t *testing.T, home string) (bool, string) {
	t.Helper()
	var out bytes.Buffer
	failed := installChecks(context.Background(), &out, nil, harness.InstallEnv{Home: home, Bin: filepath.Join(home, "bin", "crosstalk")}, false)
	return failed, out.String()
}

func TestK8NotConfiguredPasses(t *testing.T) {
	home, _, _ := statusHome(t, false)
	failed, out := statusChecks(t, home)
	if failed || installCheckLine(out, "K8", "crosstalk") != "K8 crosstalk PASS remote not configured" {
		t.Fatalf("failed=%v\n%s", failed, out)
	}
}

func TestK8BadModeFails(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		breakFile  func(*testing.T, string)
	}{
		{"key_mode", "host.key", func(t *testing.T, d string) {
			if err := os.Chmod(filepath.Join(d, "host.key"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"config_unreadable", "remote.json", func(t *testing.T, d string) {
			if err := os.Chmod(filepath.Join(d, "remote.json"), 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"certificate_mismatch", "host.crt", func(t *testing.T, d string) {
			other := filepath.Join(t.TempDir(), "other")
			if _, err := remote.LoadOrCreateHost(other); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(other, "host.crt"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "host.crt"), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"peers_mode", "peers.json", func(t *testing.T, d string) {
			if err := os.Chmod(filepath.Join(d, "peers.json"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"peers_parse", "peers.json", func(t *testing.T, d string) {
			if err := os.WriteFile(filepath.Join(d, "peers.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, dir, state := statusHome(t, true)
			statusServing(t, dir, state)
			if err := os.WriteFile(filepath.Join(dir, "peers.json"), []byte(`{"version":1,"peers":[]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if failed, out := statusChecks(t, home); failed || !strings.Contains(out, "K8 crosstalk PASS") || strings.Contains(out, "K8 crosstalk WARN") {
				t.Fatalf("control: failed=%v\n%s", failed, out)
			}
			tc.breakFile(t, dir)
			failed, out := statusChecks(t, home)
			if !failed || !strings.Contains(out, "K8 crosstalk FAIL") || !strings.Contains(out, filepath.Join(dir, tc.path)) {
				t.Fatalf("bad file: failed=%v\n%s", failed, out)
			}
		})
	}
}

func TestK8GatewayDownWarnsWithoutUnit(t *testing.T) {
	home, dir, state := statusHome(t, true)
	if failed, out := statusChecks(t, home); failed || !strings.Contains(out, "K8 crosstalk WARN") {
		t.Fatalf("without unit: failed=%v\n%s", failed, out)
	}
	p := statusUnit(t, home)
	if failed, out := statusChecks(t, home); !failed || !strings.Contains(out, "K8 crosstalk FAIL") || !strings.Contains(out, p) {
		t.Fatalf("with unit: failed=%v\n%s", failed, out)
	}
	statusServing(t, dir, state)
	if failed, out := statusChecks(t, home); failed || !strings.Contains(out, "K8 crosstalk PASS") || strings.Contains(out, "K8 crosstalk WARN") {
		t.Fatalf("running: failed=%v\n%s", failed, out)
	}
}

func TestK8BindNotOnInterfaceWarns(t *testing.T) {
	home, dir, _ := statusHome(t, true)
	c, err := remote.LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.Bind = "192.0.2.1"
	if err := remote.SaveConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	old := remote.InterfaceAddrs
	remote.InterfaceAddrs = func() ([]net.Addr, error) { return nil, nil }
	t.Cleanup(func() { remote.InterfaceAddrs = old })
	if failed, out := statusChecks(t, home); failed || !strings.Contains(out, "K8 crosstalk WARN") || !strings.Contains(out, c.Bind) {
		t.Fatalf("failed=%v\n%s", failed, out)
	}
}

func statusJSON(t *testing.T) (int, map[string]json.RawMessage, string) {
	t.Helper()
	code, out, errs := remoteRun(t, false, "status", "--json")
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("status JSON: exit=%d out=%q stderr=%q: %v", code, out, errs, err)
	}
	return code, obj, errs
}

func TestRemoteStatusStates(t *testing.T) {
	for _, want := range []string{"not configured", "not running", "running", "service running, port not bound"} {
		t.Run(want, func(t *testing.T) {
			_, dir, state := statusHome(t, want != "not configured")
			if want == "running" {
				statusServing(t, dir, state)
			}
			if want == "service running, port not bound" {
				statusLock(t, state)
			}
			_, obj, _ := statusJSON(t)
			keys := []string{"gateway_state", "alias", "bind", "port", "fingerprint", "login_unit", "peers"}
			for _, k := range keys {
				if _, ok := obj[k]; !ok {
					t.Errorf("missing key %s", k)
				}
			}
			var got string
			if err := json.Unmarshal(obj["gateway_state"], &got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("state=%q, want %q", got, want)
			}
			_, out, errs := remoteRun(t, false, "status")
			if !strings.Contains(out, "gateway: "+want+"\n") {
				t.Fatalf("text=%q stderr=%q", out, errs)
			}
			if want != "not configured" {
				c, _ := remote.LoadConfig(dir)
				h, _ := remote.LoadHost(dir)
				for key, value := range map[string]string{"alias": c.Alias, "bind": c.Bind, "fingerprint": h.Fingerprint, "login_unit": "none"} {
					var v string
					json.Unmarshal(obj[key], &v)
					if v != value || !strings.Contains(out, value) {
						t.Errorf("%s: JSON=%q text=%q want=%q", key, v, out, value)
					}
				}
				var port int
				json.Unmarshal(obj["port"], &port)
				if port != c.Port || !strings.Contains(out, fmt.Sprint(port)) {
					t.Errorf("port: %d, text=%q", port, out)
				}
			}
		})
	}
}

func statusTLSPeer(t *testing.T, stalled bool, reject bool) (remote.Host, string) {
	t.Helper()
	h, err := remote.LoadOrCreateHost(filepath.Join(t.TempDir(), "peer"))
	if err != nil {
		t.Fatal(err)
	}
	l := statusListener(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				if stalled {
					<-stop
					return
				}
				cfg := &tls.Config{Certificates: []tls.Certificate{h.Certificate}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, ClientAuth: tls.RequireAnyClientCert}
				cfg.VerifyConnection = func(cs tls.ConnectionState) error {
					if reject {
						return fmt.Errorf("unpaired")
					}
					if len(cs.PeerCertificates) == 0 {
						return fmt.Errorf("no client certificate")
					}
					return nil
				}
				s := tls.Server(c, cfg)
				s.SetDeadline(time.Now().Add(3 * time.Second))
				if s.Handshake() == nil {
					var b [1]byte
					if n, _ := s.Read(b[:]); n != 0 {
						t.Error("probe sent application bytes")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { close(stop); l.Close(); wg.Wait() })
	return h, l.Addr().String()
}

func TestRemoteStatusPeerProbe(t *testing.T) {
	for _, want := range []string{"ok", "tls_pin_mismatch", "unpaired_by_peer", "timeout", "unreachable", "parallel"} {
		t.Run(want, func(t *testing.T) {
			_, dir, _ := statusHome(t, true)
			h, addr := statusTLSPeer(t, want == "timeout" || want == "parallel", want == "unpaired_by_peer")
			fp := h.Fingerprint
			if want == "tls_pin_mismatch" {
				fp = strings.Repeat("0", 64)
			}
			if want == "unreachable" {
				l := statusListener(t)
				addr = l.Addr().String()
				l.Close()
			}
			count := 1
			if want == "parallel" {
				count = 3
			}
			ps := []remote.Peer{}
			for i := 0; i < count; i++ {
				f := fp
				if count > 1 {
					f = fmt.Sprintf("%064x", i+1)
				}
				ps = append(ps, remote.Peer{Alias: fmt.Sprintf("peer%d", i), Address: addr, Fingerprint: f})
			}
			b, err := json.Marshal(map[string]any{"version": 1, "peers": ps})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "peers.json"), b, 0o600); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			code, obj, errs := statusJSON(t)
			if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
				t.Errorf("probe took %v, limit 2.5 s", elapsed)
			}
			if code != 0 {
				t.Errorf("exit %d stderr=%q", code, errs)
			}
			var got []struct{ Alias, Address, Fingerprint, Status string }
			if err := json.Unmarshal(obj["peers"], &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != count {
				t.Fatalf("peers=%+v", got)
			}
			expected := want
			if want == "parallel" {
				expected = "timeout"
			}
			for i, p := range got {
				if p.Status != expected || p.Address != addr || p.Fingerprint != ps[i].Fingerprint || p.Alias != ps[i].Alias {
					t.Errorf("peer=%+v want status %s", p, expected)
				}
			}
		})
	}
}

func TestRemoteStatusExit(t *testing.T) {
	home, dir, state := statusHome(t, true)
	for _, args := range [][]string{{"status", "--bogus"}, {"status", "extra"}, {"status", "--json", "extra"}} {
		if code, _, errs := remoteRun(t, false, args...); code != 2 || !strings.Contains(errs, "usage:") {
			t.Errorf("args %v: exit=%d stderr=%q", args, code, errs)
		}
	}
	p := statusUnit(t, home)
	if code, _, errs := remoteRun(t, false, "status"); code != 1 || !strings.Contains(errs, p) {
		t.Errorf("down with unit: exit=%d stderr=%q", code, errs)
	}
	statusServing(t, dir, state)
	if code, _, errs := remoteRun(t, false, "status"); code != 0 {
		t.Errorf("running: exit=%d stderr=%q", code, errs)
	}
	l := statusListener(t)
	addr := l.Addr().String()
	l.Close()
	if err := remote.NewPeers(dir).Upsert(remote.Peer{Alias: "away", Address: addr, Fingerprint: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := remoteRun(t, false, "status"); code != 0 || !strings.Contains(out+errs, "unreachable") || !strings.Contains(errs, "warning") {
		t.Errorf("unreachable: exit=%d out=%q stderr=%q", code, out, errs)
	}
}

func TestRemoteStatusNeverWrites(t *testing.T) {
	for _, mode := range []string{"unconfigured", "no_state_dir", "absent_lock", "free_lock", "held_lock"} {
		t.Run(mode, func(t *testing.T) {
			home, _, state := statusHome(t, mode != "unconfigured")
			if mode != "unconfigured" && mode != "no_state_dir" {
				if err := os.MkdirAll(state, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "free_lock" || mode == "held_lock" {
				if err := os.WriteFile(filepath.Join(state, "gateway.lock"), []byte("123\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "held_lock" {
				statusLock(t, state)
			}
			snapshot := func() map[string]string {
				m := map[string]string{}
				err := filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					info, err := d.Info()
					if err != nil {
						return err
					}
					var b []byte
					if !d.IsDir() {
						b, err = os.ReadFile(p)
						if err != nil {
							return err
						}
					}
					m[p] = fmt.Sprintf("%v %d %v %s", info.Mode(), info.Size(), info.ModTime(), b)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return m
			}
			before := snapshot()
			for _, args := range [][]string{{"status"}, {"status", "--json"}} {
				if code, _, errs := remoteRun(t, false, args...); code != 0 {
					t.Errorf("exit=%d stderr=%q", code, errs)
				}
			}
			if after := snapshot(); !reflect.DeepEqual(before, after) {
				t.Errorf("files changed: before=%v after=%v", before, after)
			}
			if mode == "free_lock" {
				f, err := os.Open(filepath.Join(state, "gateway.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
					t.Errorf("status retained lock: %v", err)
				}
			}
		})
	}
}

func TestRemoteStatusBadLockPath(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink", "mode", "dir"} {
		t.Run(kind, func(t *testing.T) {
			home, _, state := statusHome(t, true)
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			lock := filepath.Join(state, "gateway.lock")
			switch kind {
			case "fifo":
				if err := unix.Mkfifo(lock, 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(home, "elsewhere"), lock); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.WriteFile(lock, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			case "dir":
				if err := os.Mkdir(lock, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			code, _, errs := remoteRun(t, false, "status")
			if time.Since(start) > time.Second || code != 1 || !strings.Contains(errs, lock) {
				t.Errorf("exit=%d after %v stderr=%q", code, time.Since(start), errs)
			}
			if _, out := statusChecks(t, home); !strings.Contains(out, "K8 crosstalk FAIL") {
				t.Errorf("K8: %s", out)
			}
		})
	}
}

func TestRemoteStatusProbeBudget(t *testing.T) {
	_, dir, _ := statusHome(t, true)
	h, err := remote.LoadHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := remote.LoadOrCreateHost(filepath.Join(t.TempDir(), "peer"))
	if err != nil {
		t.Fatal(err)
	}
	l := statusListener(t)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				time.Sleep(1850 * time.Millisecond)
				s := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{peer.Certificate}, MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAnyClientCert})
				s.SetDeadline(time.Now().Add(3 * time.Second))
				if s.Handshake() == nil {
					var b [1]byte
					s.Read(b[:])
				}
			}()
		}
	}()
	start := time.Now()
	got := remote.ProbePeer(h, remote.Peer{Alias: "slow", Address: l.Addr().String(), Fingerprint: peer.Fingerprint})
	if elapsed := time.Since(start); elapsed > 2080*time.Millisecond {
		t.Errorf("probe returned %q after %v, budget 2 s", got, elapsed)
	}
}

func TestRemoteStatusPeerClosesWithoutAlert(t *testing.T) {
	_, dir, _ := statusHome(t, true)
	h, err := remote.LoadHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := remote.LoadOrCreateHost(filepath.Join(t.TempDir(), "peer"))
	if err != nil {
		t.Fatal(err)
	}
	l := statusListener(t)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				s := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{peer.Certificate}, MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAnyClientCert})
				s.SetDeadline(time.Now().Add(3 * time.Second))
				if s.Handshake() == nil {
					c.Close()
				}
			}()
		}
	}()
	got := remote.ProbePeer(h, remote.Peer{Alias: "closer", Address: l.Addr().String(), Fingerprint: peer.Fingerprint})
	if got != "unreachable" {
		t.Errorf("peer closed with no alert: got %q, want unreachable", got)
	}
}

func TestRemoteStatusPeerTLS12OnlyIsUnreachable(t *testing.T) {
	_, dir, _ := statusHome(t, true)
	h, err := remote.LoadHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := remote.LoadOrCreateHost(filepath.Join(t.TempDir(), "peer"))
	if err != nil {
		t.Fatal(err)
	}
	l := statusListener(t)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				s := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{peer.Certificate}, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12, ClientAuth: tls.RequireAnyClientCert})
				s.SetDeadline(time.Now().Add(3 * time.Second))
				s.Handshake()
			}()
		}
	}()
	if got := remote.ProbePeer(h, remote.Peer{Alias: "old", Address: l.Addr().String(), Fingerprint: peer.Fingerprint}); got != "unreachable" {
		t.Errorf("TLS 1.2-only peer: got %q, want unreachable", got)
	}
}
