package codex

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness"
	"github.com/GoodOlClint/crosstalk/internal/harness/codex/appserver/appservertest"
)

func k9(t *testing.T, a *Adapter) harness.CheckResult {
	t.Helper()
	env, _ := codexEnv(t, "")
	h := &Harness{Threads: a}
	if a == nil {
		h = new(Harness)
	}
	for _, c := range h.Checks(context.Background(), env) {
		if c.ID == "K9" {
			return c
		}
	}
	t.Fatal("no K9 result")
	return harness.CheckResult{}
}

func TestK9WarnsWhenBackendUnreachable(t *testing.T) {
	s := appservertest.New(t)
	s.RateLimitsErr = appservertest.BackendDownError()
	c := k9(t, &Adapter{ControlSocket: s.Endpoint()})
	if c.OK || !c.WarnOnly || !strings.Contains(c.Detail, "cannot reach its OpenAI backend") || !strings.Contains(c.Detail, "daemon restart") || !strings.Contains(c.Detail, s.Endpoint()) {
		t.Errorf("K9 = %+v", c)
	}
}

func TestK9OKWithNoDaemon(t *testing.T) {
	c := k9(t, &Adapter{ControlSocket: filepath.Join(t.TempDir(), "absent.sock")})
	if !c.OK || !c.WarnOnly || c.Detail != "no managed Codex app-server running" {
		t.Errorf("K9 = %+v", c)
	}
}

func TestK9OKWhenReachable(t *testing.T) {
	s := appservertest.New(t)
	c := k9(t, &Adapter{ControlSocket: s.Endpoint()})
	if !c.OK || !c.WarnOnly || !strings.Contains(c.Detail, "reaches its OpenAI backend") || !strings.Contains(c.Detail, "model availability not checked") {
		t.Errorf("K9 = %+v", c)
	}
}

func TestK9OKWhenUnknown(t *testing.T) {
	s := appservertest.New(t)
	s.Account = "apiKey"
	c := k9(t, &Adapter{ControlSocket: s.Endpoint()})
	if !c.OK || !strings.Contains(c.Detail, "could not tell") {
		t.Errorf("K9 = %+v", c)
	}
}

func TestK9DefaultsToHomeUnderEnv(t *testing.T) {
	if c := k9(t, nil); !c.OK || c.Detail != "no managed Codex app-server running" {
		t.Errorf("K9 = %+v", c)
	}
}
