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

// GatewayHolderPID is the pid the gateway holding gateway.lock recorded in
// it, or "unknown".
func GatewayHolderPID(stateDir string) string {
	b, err := os.ReadFile(filepath.Join(stateDir, "gateway.lock"))
	if pid := strings.TrimSpace(string(b)); err == nil && pid != "" {
		return pid
	}
	return "unknown"
}

// GatewayStatus names the gateway state of a configured host. The lock says
// a gateway process runs; with its bind address gone from this host it is
// waiting for the address; the port is bound only if the listener on
// bind:port presents the certificate with this host's fingerprint, so a
// foreign listener on the address never reads as running.
func GatewayStatus(stateDir string, c Config, fingerprint string) (string, error) {
	st, _, err := GatewayStatusWhy(stateDir, c, fingerprint)
	return st, err
}

// ListenerProbe classes, ordered from least to most informative: when
// several dial addresses fail, the highest class wins.
type ListenerProbe int

const (
	ProbeRefused         ListenerProbe = iota // nothing accepted the connection
	ProbeHandshakeFailed                      // connected, but no TLS 1.3 certificate came back
	ProbeMismatch                             // a certificate came back with another fingerprint
	ProbeOurs
)

// GatewayStatusWhy is GatewayStatus plus, for StateNoPort and
// StateWaitingAddr, how the probe of the listener failed.
func GatewayStatusWhy(stateDir string, c Config, fingerprint string) (string, ListenerProbe, error) {
	held, err := GatewayLockHeld(stateDir)
	if err != nil {
		return "", ProbeRefused, err
	}
	if !held {
		return StateNotRunning, ProbeRefused, nil
	}
	best := ProbeRefused
	for _, h := range DialBinds(c.Bind) {
		pr := probeListener(net.JoinHostPort(h, strconv.Itoa(c.Port)), fingerprint)
		if pr == ProbeOurs {
			return StateRunning, pr, nil
		}
		best = max(best, pr)
	}
	if !BindOnHost(c.Bind) {
		return StateWaitingAddr, best, nil
	}
	return StateNoPort, best, nil
}

// probeListener starts a TLS 1.3 handshake with no client certificate and
// classifies what answered: nothing, a peer that cut the handshake, a
// certificate with another fingerprint, or the certificate with fingerprint.
func probeListener(addr, fingerprint string) ListenerProbe {
	deadline := time.Now().Add(probeTimeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return ProbeRefused
	}
	defer raw.Close()
	raw.SetDeadline(deadline)
	seen, ours := false, false
	c := tls.Client(raw, &tls.Config{
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			seen = len(cs.PeerCertificates) > 0
			ours = seen && Fingerprint(cs.PeerCertificates[0]) == fingerprint
			return nil
		},
	})
	err = c.HandshakeContext(ctx)
	switch {
	case seen && !ours:
		return ProbeMismatch
	case err != nil || !seen:
		return ProbeHandshakeFailed
	}
	return ProbeOurs
}

// ProbePeer completes a pinned TLS 1.3 handshake with p using this host's
// certificate and sends no application bytes. It returns ok,
// tls_pin_mismatch, unpaired_by_peer, timeout or unreachable.
func ProbePeer(h Host, p Peer) string {
	deadline := time.Now().Add(probeTimeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	raw, err := DialPeer(ctx, "tcp", p.Address)
	if err != nil {
		return probeClass(err)
	}
	defer raw.Close()
	c := tls.Client(raw, pinnedTLSConfig(h, p, nil))
	raw.SetDeadline(deadline)
	if err := c.HandshakeContext(ctx); err != nil {
		if handshakeCut(err) {
			return ReasonTimeout
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
		return probeClass(err)
	}
	return "ok"
}

// probeClass is Classify with a refused connection reported as unreachable:
// the probe has no gateway_not_running state.
func probeClass(err error) string {
	if r := Classify(err); r != ReasonGatewayNotRunning {
		return r
	}
	return ReasonUnreachable
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
		"unknown certificate", "unknown certificate authority", "certificate required", "access denied":
		return true
	}
	return false
}
