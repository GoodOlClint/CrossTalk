package remote

import (
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func holdLock(t *testing.T, stateDir string) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "gateway.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

func foreignListener(t *testing.T, tlsCfg *tls.Config) Config {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg != nil {
		l = tls.NewListener(l, tlsCfg)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	return Config{Alias: "desk", Bind: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port}
}

func TestGatewayStatusForeignListener(t *testing.T) {
	host, err := LoadOrCreateHost(newDir(t))
	if err != nil {
		t.Fatal(err)
	}
	other, err := LoadOrCreateHost(newDir(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		lock bool
		tls  *tls.Config
		want string
	}{
		{"service up, plain TCP listener holds the port", true, nil, StateNoPort},
		{"service up, TLS listener with another certificate holds the port", true, &tls.Config{Certificates: []tls.Certificate{other.Certificate}, MinVersion: tls.VersionTLS13}, StateNoPort},
		{"service up, listener presents this host's certificate without its key", true, &tls.Config{Certificates: []tls.Certificate{{Certificate: host.Certificate.Certificate, PrivateKey: other.Certificate.PrivateKey}}, MinVersion: tls.VersionTLS13}, StateNoPort},
		{"no lock, plain TCP listener holds the port", false, nil, StateNotRunning},
		{"no lock, TLS listener with another certificate", false, &tls.Config{Certificates: []tls.Certificate{other.Certificate}, MinVersion: tls.VersionTLS13}, StateNotRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := foreignListener(t, tc.tls)
			state := filepath.Join(t.TempDir(), "state")
			if tc.lock {
				holdLock(t, state)
			}
			got, err := GatewayStatus(state, cfg, host.Fingerprint)
			if err != nil || got != tc.want || got == StateRunning {
				t.Errorf("GatewayStatus = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestGatewayStatusRunningNeedsOwnCertificate(t *testing.T) {
	e := startGateway(t, nil)
	cfg := Config{Alias: "desk", Bind: "127.0.0.1", Port: e.gw.Addr().(*net.TCPAddr).Port}
	if got, err := GatewayStatus(e.stateDir, cfg, e.host.Fingerprint); err != nil || got != StateRunning {
		t.Errorf("GatewayStatus with this host's gateway = %q, %v; want %q", got, err, StateRunning)
	}
	if got, err := GatewayStatus(e.stateDir, cfg, "00"+e.host.Fingerprint[2:]); err != nil || got != StateNoPort {
		t.Errorf("GatewayStatus when the listener presents another key = %q, %v; want %q", got, err, StateNoPort)
	}
}
