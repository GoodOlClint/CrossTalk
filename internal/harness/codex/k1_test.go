package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func k1k2(t *testing.T, binVer string, installed ...string) (k1, k2 bool, detail string) {
	t.Helper()
	env, _ := codexEnv(t, "")
	env.Version = binVer
	for _, v := range installed {
		p := pluginManifest(env.Home, v)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(`{"name":"crosstalk","version":"`+v+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range new(Harness).Checks(context.Background(), env) {
		switch c.ID {
		case "K1":
			k1, detail = c.OK, c.Detail
		case "K2":
			k2 = c.OK
		}
	}
	return
}

func TestK1AcceptsRCBinaryWithPreviousPlugin(t *testing.T) {
	k1, k2, d := k1k2(t, "0.1.4-rc.1", "0.1.3")
	if !k1 || !k2 || !strings.Contains(d, "CROSSTALK_MARKETPLACE") {
		t.Errorf("k1=%v k2=%v detail=%q", k1, k2, d)
	}
}

func TestK1RCBinaryPicksNewestAcceptableCache(t *testing.T) {
	if k1, _, d := k1k2(t, "0.1.4-rc.1", "0.1.2", "0.1.3"); !k1 || !strings.Contains(d, "0.1.3") {
		t.Errorf("k1=%v detail=%q", k1, d)
	}
}

func TestK1FinalBinaryMismatchStillFails(t *testing.T) {
	if k1, k2, _ := k1k2(t, "0.1.4", "0.1.3"); k1 || k2 {
		t.Errorf("final binary accepted an older plugin: k1=%v k2=%v", k1, k2)
	}
}

func TestK1RCBinaryRejectsManifestWithoutVersion(t *testing.T) {
	env, _ := codexEnv(t, "")
	env.Version = "0.1.4-rc.1"
	p := pluginManifest(env.Home, "0.1.3")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(`{"name":"crosstalk"}`), 0o644)
	for _, c := range new(Harness).Checks(context.Background(), env) {
		if c.ID == "K1" && c.OK {
			t.Errorf("K1 passed a manifest with no version: %s", c.Detail)
		}
	}
}
