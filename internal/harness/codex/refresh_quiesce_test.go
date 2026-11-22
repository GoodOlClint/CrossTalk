package codex

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/quiesce"
)

type blockingRegistrar struct {
	fakeRegistrar
	in, out chan struct{}
}

func (r *blockingRegistrar) RegisterSession(ctx context.Context, req harness.RequestContext, cwd string) error {
	close(r.in)
	<-r.out
	return r.fakeRegistrar.RegisterSession(ctx, req, cwd)
}

// A startup claim (the first durable scan, or a carried claim on resume) is
// counted through its registration, not only through the proof: a refresh
// never snapshots the agent without its proven claim.
func TestClaimRegistrationHoldsQuiesce(t *testing.T) {
	for _, resume := range []bool{false, true} {
		e := newHookEnv(t, true)
		setRefreshLock(e.sys, "T1", parentPID)
		parent, _ := e.sys.Info(parentPID)
		reg := &blockingRegistrar{in: make(chan struct{}), out: make(chan struct{})}
		c := NewClaims(ClaimOptions{Sys: e.sys, Parent: parent, LockDir: refreshLockDir, Registrar: reg, Tries: 1, Sleep: func(time.Duration) {}})
		done := make(chan struct{})
		go func() {
			if resume {
				c.Resume(context.Background(), []Binding{{Claim: claim("T1")}})
			} else {
				c.AcceptDurable(context.Background(), claim("T1"))
			}
			close(done)
		}()
		<-reg.in
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		err := quiesce.Wait(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("resume=%v: quiescent during the claim's registration: %v", resume, err)
		}
		close(reg.out)
		<-done
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		if err := quiesce.Wait(ctx); err != nil {
			t.Fatalf("resume=%v: busy after the registration: %v", resume, err)
		}
		cancel()
	}
}
