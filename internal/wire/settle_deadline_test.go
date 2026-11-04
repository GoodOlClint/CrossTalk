package wire

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/GoodOlClint/crosstalk/internal/adapter"
	"github.com/GoodOlClint/crosstalk/internal/msg"
)

func TestSettlePollBoundedWhenGetTaskNeverAnswers(t *testing.T) {
	dir, alice, bob := pair(t)
	target, err := url.Parse(descriptor(t, dir, bob.id()).A2AURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"GetTask"`)) {
			<-r.Context().Done()
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer hang.Close()

	to := descriptor(t, dir, bob.id())
	to.A2AURL = hang.URL + target.Path
	alice.w.settle = 300 * time.Millisecond
	m := msg.Message{
		MessageID: "019a3b7e-0000-7000-8000-000000000001", ContextID: "ctx", From: "alice", To: "bob",
		Kind: msg.Inform, Delivery: msg.Immediate, SentAt: time.Now(), Parts: []msg.Part{{Type: "text", Text: "hi"}},
	}
	start := time.Now()
	_, state, err := alice.w.SendTask(context.Background(), descriptor(t, dir, alice.id()), to, m, true)
	if el := time.Since(start); el > alice.w.settle+500*time.Millisecond {
		t.Fatalf("settle poll took %v, budget %v", el, alice.w.settle)
	}
	if state != adapter.Unknown || err == nil || err.Error() != ReasonPending {
		t.Fatalf("state %v err %v, want unknown/%s", state, err, ReasonPending)
	}
}
