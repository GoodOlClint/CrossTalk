package remote

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"
)

func alertServer(t *testing.T, code byte) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				buf := make([]byte, 4096)
				c.Read(buf)
				c.Write([]byte{21, 3, 3, 0, 2, 2, code})
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func joinAgainst(t *testing.T, port int) error {
	t.Helper()
	h, err := LoadOrCreateHost(newDir(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = JoinGateway(context.Background(), JoinOptions{
		Host: h, Config: Config{Version: 1, Alias: "beta", Bind: "127.0.0.1", Port: 7746}, Peers: NewPeers(newDir(t)),
		Token:   PairToken{Address: "127.0.0.1", Port: port, Fingerprint: strings.Repeat("cd", 32), ID: "id", Secret: "c2VjcmV0"},
		Timeout: 3 * time.Second,
	})
	if err == nil {
		t.Fatal("join succeeded")
	}
	return err
}

const notWaiting = "is not waiting for a pairing"

func TestJoinNonRefusalAlertIsNotAPairingRefusal(t *testing.T) {
	err := joinAgainst(t, alertServer(t, 113))
	if strings.Contains(err.Error(), notWaiting) {
		t.Fatalf("a bad certificate status response alert was read as a refusal: %v", err)
	}
}

func TestJoinRefusalAlertSaysNotWaiting(t *testing.T) {
	err := joinAgainst(t, alertServer(t, 42))
	if !strings.Contains(err.Error(), notWaiting) {
		t.Fatalf("bad certificate alert: %v", err)
	}
}

func TestJoinTLS12PeerIsNotAPairingRefusal(t *testing.T) {
	h, err := LoadOrCreateHost(newDir(t))
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{h.Certificate}, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	err = joinAgainst(t, l.Addr().(*net.TCPAddr).Port)
	if strings.Contains(err.Error(), notWaiting) {
		t.Fatalf("TLS 1.2 peer: %v", err)
	}
}
