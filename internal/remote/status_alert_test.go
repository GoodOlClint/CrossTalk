package remote

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestIsAlertClassifiesCertificateRefusals(t *testing.T) {
	remoteErr := func(msg string) error { return &net.OpError{Op: "remote error", Err: errors.New("tls: " + msg)} }
	for msg, want := range map[string]bool{
		"bad certificate":                 true,
		"unsupported certificate":         true,
		"revoked certificate":             true,
		"expired certificate":             true,
		"unknown certificate":             true,
		"unknown certificate authority":   true,
		"certificate required":            true,
		"handshake failure":               false,
		"access denied":                   true,
		"protocol version not supported":  false,
		"internal error":                  false,
		"certificate unobtainable":        false,
		"bad certificate status response": false,
	} {
		if got := isAlert(remoteErr(msg)); got != want {
			t.Errorf("isAlert(%q) = %v, want %v", msg, got, want)
		}
	}
	if isAlert(errors.New("tls: bad certificate")) || isAlert(&net.OpError{Op: "read", Err: errors.New("tls: bad certificate")}) {
		t.Error("isAlert accepted an error that is not a remote alert")
	}
}

func TestClassifyUsesExactAlerts(t *testing.T) {
	remoteErr := func(msg string) error { return &net.OpError{Op: "remote error", Err: errors.New("tls: " + msg)} }
	for msg, want := range map[string]string{
		"bad certificate":                 ReasonUnpairedByPeer,
		"certificate required":            ReasonUnpairedByPeer,
		"handshake failure":               ReasonUnreachable,
		"bad certificate status response": ReasonUnreachable,
		"not a bad certificate":           ReasonUnreachable,
	} {
		if got := Classify(remoteErr(msg)); got != want {
			t.Errorf("Classify(%q) = %s, want %s", msg, got, want)
		}
	}
}

func TestOnePinnedTLSConfig(t *testing.T) {
	h, err := LoadOrCreateHost(filepath.Join(t.TempDir(), "crosstalk"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := pinnedTLSConfig(h, Peer{Fingerprint: h.Fingerprint}, nil)
	if cfg.MinVersion != tls.VersionTLS13 || cfg.MaxVersion != tls.VersionTLS13 || len(cfg.Certificates) != 1 {
		t.Errorf("pinned config: min %x max %x certs %d", cfg.MinVersion, cfg.MaxVersion, len(cfg.Certificates))
	}
}

func TestHTTPClientCapsResponseBody(t *testing.T) {
	h, err := LoadOrCreateHost(filepath.Join(t.TempDir(), "crosstalk"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), MaxResponse+10))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{h.Certificate}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()
	resp, err := NewClient(h, Peer{Address: srv.Listener.Addr().String(), Fingerprint: h.Fingerprint}).HTTPClient().Get("https://" + srv.Listener.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); !errors.Is(err, errTooLarge) {
		t.Fatalf("reading an over-cap body: %v, want errTooLarge", err)
	}
}
