package remote

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/identity"
	"github.com/GoodOlClint/crosstalk/internal/msg"
	"github.com/GoodOlClint/crosstalk/internal/registry"
)

const (
	NudgeBudget     = 12 * time.Second
	nudgeMaxRows    = 50
	nudgeSenderName = "crosstalk-remote-config"
)

// AddressChanged reports whether next moves the gateway's dialled address:
// the advertised address or the port. A first configure is not a change.
func AddressChanged(old, next Config, configured bool) bool {
	return configured && (old.AdvertiseAddr() != next.AdvertiseAddr() || old.Port != next.Port)
}

// NudgeSender is the sender of the peer nudge: stable per host, never a
// session. Its name avoids every name in taken.
func NudgeSender(fingerprint string, taken func(string) bool) registry.Descriptor {
	name := nudgeSenderName
	for i := 2; taken != nil && taken(name); i++ {
		name = nudgeSenderName + "-" + strconv.Itoa(i)
	}
	return registry.Descriptor{
		AgentID: identity.AgentID("claude", "remote-config-nudge:"+fingerprint),
		Name:    name, Harness: "claude", Role: registry.RolePrimary,
	}
}

func hostPort(c Config) string {
	return net.JoinHostPort(c.AdvertiseAddr(), strconv.Itoa(c.Port))
}

// LocalNudgeText tells a session on this host that the gateway moved.
func LocalNudgeText(old, next Config) string {
	return fmt.Sprintf("CrossTalk notice from this host's `crosstalk remote config`, not from a session: the gateway address changed from %s to %s. Paired hosts still dial the old address and may show as unreachable until they pair again with `crosstalk remote pair` and `crosstalk remote join`. This is information only; nothing is asked of you.", hostPort(old), hostPort(next))
}

// PeerNudgeText tells a session on a paired host that this host moved.
func PeerNudgeText(alias string, old, next Config) string {
	return fmt.Sprintf("CrossTalk notice from the `crosstalk remote config` command on host %s, not from a session: that host's gateway address changed from %s to %s. Its sessions may show as unreachable from this host until the two hosts pair again. This is information only; nothing is asked of you.", alias, hostPort(old), hostPort(next))
}

// NudgeDeps are the effects the nudge needs; tests replace them.
type NudgeDeps struct {
	Locals     []registry.Descriptor
	SendLocal  func(d registry.Descriptor, text string) error
	Host       Host
	Peers      []Peer
	Rows       func(ctx context.Context, h Host, p Peer) ([]AgentRow, error)
	SendRemote func(ctx context.Context, from registry.Descriptor, h Host, p Peer, agentID string, m msg.Message) error
	Timeout    time.Duration
	Now        func() time.Time
}

// SendAddressNudge delivers the address-change nudge to this host's sessions
// and to the sessions of every paired host, all concurrently under one
// deadline. A peer that cannot be reached gets nothing and never delays the
// local sends. It returns when every send finished or the deadline passed.
func SendAddressNudge(ctx context.Context, d NudgeDeps, aliasOfThisHost string, old, next Config) {
	if d.Timeout <= 0 {
		d.Timeout = NudgeBudget
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()

	var wg sync.WaitGroup
	text := LocalNudgeText(old, next)
	for _, l := range d.Locals {
		if l.UDSPath == "" {
			continue
		}
		wg.Add(1)
		go func(l registry.Descriptor) {
			defer wg.Done()
			d.SendLocal(l, text)
		}(l)
	}
	taken := map[string]bool{}
	for _, l := range d.Locals {
		taken[l.Name] = true
	}
	from := NudgeSender(d.Host.Fingerprint, func(n string) bool { return taken[n] })
	ptext := PeerNudgeText(aliasOfThisHost, old, next)
	for _, p := range d.Peers {
		wg.Add(1)
		go func(p Peer) {
			defer wg.Done()
			rows, err := d.Rows(ctx, d.Host, p)
			if err != nil {
				return
			}
			var inner sync.WaitGroup
			n := 0
			for _, r := range rows {
				if !r.CrossTalk || !r.Messageable {
					continue
				}
				if n++; n > nudgeMaxRows {
					break
				}
				inner.Add(1)
				go func(r AgentRow) {
					defer inner.Done()
					id := msg.NewUUIDv7()
					d.SendRemote(ctx, from, d.Host, p, r.AgentID, msg.Message{
						MessageID: id, ContextID: id, From: from.Name, To: r.AgentID + "@" + p.Alias,
						Kind: msg.Inform, Delivery: msg.Immediate,
						HopChain: []string{msg.Hop(from.AgentID)}, SentAt: d.Now(),
						Parts: []msg.Part{{Type: "text", Text: ptext}},
					})
				}(r)
			}
			inner.Wait()
		}(p)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
