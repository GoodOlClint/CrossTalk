package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoodOlClint/crosstalk/internal/harness"
)

func TestIdentifyValidatesSessionFile(t *testing.T) {
	dir := t.TempDir()
	h, err := NewHarness(HarnessOptions{SessionsDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	rc := harness.RequestContext{ParentPID: 4242}
	identify := func() (harness.Session, bool, error) { return h.Identify(context.Background(), rc) }
	if _, ok, err := identify(); ok || err != nil {
		t.Fatalf("missing file: ok=%v err=%v, want unbound without error", ok, err)
	}
	path := filepath.Join(dir, "4242.json")
	for name, tc := range map[string]struct {
		body    string
		wantErr string
	}{
		"valid":     {`{"pid":4242,"sessionId":"S","name":"alpha"}`, ""},
		"wrong pid": {`{"pid":1,"sessionId":"S"}`, "claude: session file 4242.json names pid 1"},
		"empty id":  {`{"pid":4242,"sessionId":""}`, "claude: session file 4242.json names pid 4242"},
		"not json":  {`{nope`, "claude: session file 4242.json: "},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			s, ok, err := identify()
			if tc.wantErr == "" {
				if err != nil || !ok || s.SessionRef != "S" || s.HarnessPID != 4242 {
					t.Fatalf("got %+v ok=%v err=%v", s, ok, err)
				}
				return
			}
			if ok || err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Fatalf("ok=%v err=%v, want error prefix %q", ok, err, tc.wantErr)
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		if err := os.WriteFile(path, []byte(`{"pid":4242,"sessionId":"S"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := os.ReadFile(path); err == nil {
			t.Skip("file mode 000 is readable (root)")
		}
		if _, ok, err := identify(); ok || err == nil || !strings.HasPrefix(err.Error(), "claude: session file 4242.json: ") {
			t.Fatalf("ok=%v err=%v, want the <pid>.json wrap", ok, err)
		}
	})
}
