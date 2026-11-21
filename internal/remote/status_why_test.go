package remote

import (
	"crypto/tls"
	"net"
	"path/filepath"
	"testing"
)

func TestGatewayStatusWhyClasses(t *testing.T) {
	host, err := LoadOrCreateHost(newDir(t))
	if err != nil {
		t.Fatal(err)
	}
	other, err := LoadOrCreateHost(newDir(t))
	if err != nil {
		t.Fatal(err)
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := Config{Alias: "desk", Bind: "127.0.0.1", Port: closed.Addr().(*net.TCPAddr).Port}
	closed.Close()
	tls13 := func(h Host) *tls.Config {
		return &tls.Config{Certificates: []tls.Certificate{h.Certificate}, MinVersion: tls.VersionTLS13}
	}
	cases := []struct {
		name string
		cfg  Config
		want ListenerProbe
	}{
		{"nothing listens", refused, ProbeRefused},
		{"plain TCP listener cuts the handshake", foreignListener(t, nil), ProbeHandshakeFailed},
		{"TLS listener with another certificate", foreignListener(t, tls13(other)), ProbeMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			holdLock(t, state)
			got, why, err := GatewayStatusWhy(state, tc.cfg, host.Fingerprint)
			if err != nil || got != StateNoPort || why != tc.want {
				t.Errorf("GatewayStatusWhy = %q, %v, %v; want %q, %v", got, why, err, StateNoPort, tc.want)
			}
		})
	}
}

func TestGatewayStatusWhyRunning(t *testing.T) {
	e := startGateway(t, nil)
	cfg := Config{Alias: "desk", Bind: "127.0.0.1", Port: e.gw.Addr().(*net.TCPAddr).Port}
	if got, why, err := GatewayStatusWhy(e.stateDir, cfg, e.host.Fingerprint); err != nil || got != StateRunning || why != ProbeOurs {
		t.Errorf("GatewayStatusWhy = %q, %v, %v", got, why, err)
	}
}
