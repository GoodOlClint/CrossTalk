package remote

import (
	"crypto/tls"
	"errors"
	"net"
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
