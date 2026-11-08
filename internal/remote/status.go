package remote

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	StateNotConfigured = "not configured"
	StateNotRunning    = "not running"
	StateRunning       = "running"
	StateNoPort        = "service running, port not bound"

	probeTimeout = 2 * time.Second
	alertWait    = 300 * time.Millisecond
)

// GatewayLockHeld reports whether a gateway holds gateway.lock. It never
// creates the file and drops any lock it briefly gets.
func GatewayLockHeld(stateDir string) (bool, error) {
	path := filepath.Join(stateDir, "gateway.lock")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remote: %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		return false, fmt.Errorf("%w: %s is %v, want a regular file with mode 0600", ErrMode, path, fi.Mode())
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
	unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return false, nil
}

// GatewayStatus names the gateway state of a configured host from the lock
// and a TCP connect to bind:port; it speaks no protocol to the gateway.
func GatewayStatus(stateDir string, c Config) (string, error) {
	held, err := GatewayLockHeld(stateDir)
	if err != nil {
		return "", err
	}
	if !held {
		return StateNotRunning, nil
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(c.Bind, strconv.Itoa(c.Port)), probeTimeout)
	if err != nil {
		return StateNoPort, nil
	}
	conn.Close()
	return StateRunning, nil
}

// ProbePeer completes a pinned TLS 1.3 handshake with p using this host's
// certificate and sends no application bytes. It returns ok,
// tls_pin_mismatch, unpaired_by_peer, timeout or unreachable.
func ProbePeer(h Host, p Peer) string {
	deadline := time.Now().Add(probeTimeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.Address)
	if err != nil {
		return probeClass(err)
	}
	defer raw.Close()
	mismatch := false
	c := tls.Client(raw, &tls.Config{
		Certificates:       []tls.Certificate{h.Certificate},
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || Fingerprint(cs.PeerCertificates[0]) != p.Fingerprint {
				mismatch = true
				return errors.New("peer certificate does not match the pinned fingerprint")
			}
			return nil
		},
	})
	raw.SetDeadline(deadline)
	if err := c.HandshakeContext(ctx); err != nil {
		if mismatch {
			return "tls_pin_mismatch"
		}
		if isAlert(err) {
			return "unpaired_by_peer"
		}
		return probeClass(err)
	}
	// TLS 1.3 finishes the client side before the server judges our
	// certificate, so a refusal arrives as an alert on the next read.
	wait := time.Now().Add(alertWait)
	if wait.After(deadline) {
		wait = deadline
	}
	raw.SetReadDeadline(wait)
	var b [1]byte
	if _, err := c.Read(b[:]); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return "ok"
		}
		if isAlert(err) {
			return "unpaired_by_peer"
		}
		return "unreachable"
	}
	return "ok"
}

func probeClass(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "unreachable"
}

// isAlert reports whether err is a TLS alert from the peer that refuses our
// certificate, as opposed to a plain close, reset, timeout or an unrelated
// alert such as a protocol version mismatch.
func isAlert(err error) bool {
	var oe *net.OpError
	if !errors.As(err, &oe) || oe.Op != "remote error" {
		return false
	}
	switch strings.TrimPrefix(oe.Err.Error(), "tls: ") {
	case "bad certificate", "unsupported certificate", "revoked certificate", "expired certificate",
		"unknown certificate", "unknown certificate authority", "certificate required", "handshake failure", "access denied":
		return true
	}
	return false
}
