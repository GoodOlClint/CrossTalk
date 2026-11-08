package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/remote"
)

func TestStatusReportsStalledHandshake(t *testing.T) {
	for _, closeAtOnce := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%v", closeAtOnce), func(t *testing.T) {
			home, dir, _ := statusHome(t, true)
			_ = home
			l := statusListener(t)
			var held []net.Conn
			var mu sync.Mutex
			go func() {
				for {
					c, err := l.Accept()
					if err != nil {
						return
					}
					if closeAtOnce {
						c.Close()
					} else {
						mu.Lock()
						held = append(held, c)
						mu.Unlock()
					}
				}
			}()
			t.Cleanup(func() {
				l.Close()
				mu.Lock()
				defer mu.Unlock()
				for _, c := range held {
					c.Close()
				}
			})
			if err := remote.NewPeers(dir).Upsert(remote.Peer{Alias: "lab", Address: l.Addr().String(), Fingerprint: strings.Repeat("ab", 32)}); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			start := time.Now()
			runRemote([]string{"status"}, strings.NewReader(""), false, &out, &errb)
			if time.Since(start) > 5*time.Second {
				t.Fatalf("status took %v", time.Since(start))
			}
			s := errb.String()
			if !strings.Contains(s, "handshake") || !strings.Contains(s, "a host firewall or network filter (for example an application firewall such as Little Snitch) on either host may be holding the connection") {
				t.Fatalf("stderr %q lacks the word handshake or the firewall hint", s)
			}
			if !strings.Contains(out.String(), "peer lab "+l.Addr().String()) || !strings.Contains(out.String(), ": timeout") {
				t.Fatalf("stdout %q, want the peer line to end in timeout", out.String())
			}
		})
	}
}

func TestPairPrintsFirewallNote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if code, _, _ := remoteRun(t, false, "config", "--bind", "127.0.0.1", "--alias", "alpha", "--yes"); code != 0 {
		t.Fatal("config failed")
	}
	dir := filepath.Join(home, ".config", "crosstalk")
	state := filepath.Join(home, ".local", "state", "crosstalk")
	host, err := remote.LoadHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	gw := remote.NewGateway(remote.GatewayConfig{StateDir: state, Host: host, Bind: "127.0.0.1", Alias: "alpha", Peers: remote.NewPeers(dir)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gw.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-gw.Ready():
	case err := <-done:
		done <- err
		t.Fatal(err)
	}
	var out, errb syncBuf
	exit := make(chan int, 1)
	go func() { exit <- runRemote([]string{"pair"}, strings.NewReader(""), false, &out, &errb) }()
	var tok remote.PairToken
	for deadline := time.Now().Add(5 * time.Second); ; {
		line, _, _ := strings.Cut(out.String(), "\n")
		if t2, err := remote.ParsePairToken(line); err == nil {
			tok = t2
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no token printed: out %q err %q", out.String(), errb.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := "pair waits up to 10 minutes; if crosstalk remote join on the other host reports a handshake timeout, check firewalls or network filters on this host\n"
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(errb.String(), want); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Errorf("stderr %q lacks %q", errb.String(), want)
			break
		}
	}
	tok.Port = gw.Addr().(*net.TCPAddr).Port
	bdir := filepath.Join(t.TempDir(), "crosstalk")
	bhost, err := remote.LoadOrCreateHost(bdir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.JoinGateway(context.Background(), remote.JoinOptions{
		Host: bhost, Config: remote.Config{Version: 1, Alias: "beta", Bind: "127.0.0.1", Port: 7746}, Peers: remote.NewPeers(bdir), Token: tok, Timeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("join: %v", err)
	}
	select {
	case <-exit:
	case <-time.After(5 * time.Second):
		t.Fatal("pair did not finish")
	}
}

func TestSetupDocStalledHandshake(t *testing.T) {
	doc := repoFile(t, "docs/setup.md")
	for _, want := range []string{
		"arrives in 0.2.0 and is not yet released",
		"handshake not completed from", "handshake timeout",
		"a host firewall or network filter", "Little Snitch",
		"pair waits the full 10 minutes",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("setup.md lacks %q", want)
		}
	}
	if strings.Contains(doc, "A stalled handshake is silent") {
		t.Error("setup.md still says a stalled handshake is silent")
	}
	plan := repoFile(t, "docs/plans/release-0.2.0.md")
	if !strings.Contains(plan, "R12 stalled-handshake report (#83)") {
		t.Error("release-0.2.0.md lacks the R12 status line for #83")
	}
}

func TestJoinPrintsStalledHandshakeHint(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { <-stop; c.Close() }()
		}
	}()
	t.Cleanup(func() { close(stop); l.Close() })
	t.Setenv("HOME", t.TempDir())
	if code, _, _ := remoteRun(t, false, "config", "--bind", "127.0.0.1", "--alias", "beta", "--yes"); code != 0 {
		t.Fatal("config failed")
	}
	port := l.Addr().(*net.TCPAddr).Port
	tok := remote.PairToken{Address: "127.0.0.1", Port: port, Fingerprint: strings.Repeat("cd", 32), ID: base64.RawURLEncoding.EncodeToString(make([]byte, 16)), Secret: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}.String()
	var out, errb bytes.Buffer
	if code := runRemote([]string{"join"}, strings.NewReader(tok+"\n"), false, &out, &errb); code != 1 {
		t.Fatalf("join exit %d, stderr %q", code, errb.String())
	}
	want := "crosstalk remote join: remote: handshake timeout: no TLS handshake completed with 127.0.0.1:" + fmt.Sprint(port) + "; a host firewall or network filter (for example an application firewall such as Little Snitch) on either host may be holding the connection\n"
	if errb.String() != want {
		t.Fatalf("stderr %q, want %q", errb.String(), want)
	}
}
