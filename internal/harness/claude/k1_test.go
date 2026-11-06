package claude

import (
	"context"
	"strings"
	"testing"
)

func k1(t *testing.T, binVer, pluginVer string) (bool, string) {
	t.Helper()
	r := &recorder{plugins: `[{"id":"crosstalk@crosstalk","version":"` + pluginVer + `"}]`}
	env := installEnv(t, r)
	env.Version = binVer
	for _, c := range new(Harness).Checks(context.Background(), env) {
		if c.ID == "K1" {
			return c.OK, c.Detail
		}
	}
	t.Fatal("no K1")
	return false, ""
}

func TestK1AcceptsRCBinaryWithPreviousPlugin(t *testing.T) {
	for _, plugin := range []string{"0.1.3", "0.1.4"} {
		ok, d := k1(t, "0.1.4-rc.1", plugin)
		if !ok || !strings.Contains(d, "CROSSTALK_MARKETPLACE") {
			t.Errorf("plugin %s: ok=%v detail=%q", plugin, ok, d)
		}
	}
}

func TestK1RCBinaryRejectsNewerPlugin(t *testing.T) {
	if ok, _ := k1(t, "0.1.4-rc.1", "0.1.5"); ok {
		t.Error("rc binary accepted a newer plugin")
	}
}

func TestK1FinalBinaryMismatchStillFails(t *testing.T) {
	if ok, _ := k1(t, "0.1.4", "0.1.3"); ok {
		t.Error("final binary accepted an older plugin")
	}
	if ok, _ := k1(t, "0.1.4", "0.1.4"); !ok {
		t.Error("matching versions failed")
	}
}

func TestK1RCBinaryRejectsMalformedPluginVersion(t *testing.T) {
	for _, v := range []string{"", "garbage", "0.1", "0.1.x", "0.1.3-beta"} {
		if ok, _ := k1(t, "0.1.4-rc.1", v); ok {
			t.Errorf("rc binary accepted plugin version %q", v)
		}
	}
}
