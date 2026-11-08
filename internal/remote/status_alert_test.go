package remote

import (
	"errors"
	"net"
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
		"handshake failure":               true,
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
