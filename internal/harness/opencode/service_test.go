package opencode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPassword = "PW-s3cr3t-Zk9Qx"

func writeService(t *testing.T, dir, url, password string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, "service.json")
	body := fmt.Sprintf(`{"id":"svc_1","version":"2.4.0","url":%q,"pid":4242,"password":%q}`, url, password)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func noPassword(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, testPassword) {
		t.Errorf("%s contains the password: %q", what, s)
	}
}

func TestReadServiceOK(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:49374", "http://[::1]:49374"} {
		path := writeService(t, t.TempDir(), u, testPassword, 0o600)
		s, err := ReadService(path)
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		if s.Origin != u || s.PID != 4242 || s.Version != "2.4.0" {
			t.Errorf("%s: got %+v", u, s)
		}
	}
}

func TestReadServiceRefusals(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, dir string) string
		reason Reason
	}{
		{"missing", func(t *testing.T, dir string) string { return filepath.Join(dir, "service.json") }, ReasonMissing},
		{"mode 0644", func(t *testing.T, dir string) string {
			return writeService(t, dir, "http://127.0.0.1:1", testPassword, 0o644)
		}, ReasonMode},
		{"mode 0660", func(t *testing.T, dir string) string {
			return writeService(t, dir, "http://127.0.0.1:1", testPassword, 0o660)
		}, ReasonMode},
		{"symlink", func(t *testing.T, dir string) string {
			real := writeService(t, dir, "http://127.0.0.1:1", testPassword, 0o600)
			link := filepath.Join(dir, "link.json")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			return link
		}, ReasonSymlink},
		{"directory", func(t *testing.T, dir string) string {
			d := filepath.Join(dir, "service.json")
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			return d
		}, ReasonNotRegular},
		{"bad json", func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "service.json")
			if err := os.WriteFile(p, []byte(`{"password":"`+testPassword+`", oops`), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}, ReasonJSON},
		{"empty password", func(t *testing.T, dir string) string {
			return writeService(t, dir, "http://127.0.0.1:1", "", 0o600)
		}, ReasonPassword},
	}
	for _, u := range []string{
		"http://192.168.1.5:49374", "http://localhost:49374", "http://example.com:49374",
		"http://0.0.0.0:49374", "http://127.0.0.2:49374", "https://127.0.0.1:49374",
		"http://user:pw@127.0.0.1:49374", "http://127.0.0.1:49374/api", "http://127.0.0.1:49374/?x=1",
		"http://127.0.0.1", "http://127.0.0.1:0", "http://[::ffff:127.0.0.1]:49374", "http://[fe80::1%25en0]:49374", "ftp://127.0.0.1:1", "",
	} {
		u := u
		cases = append(cases, struct {
			name   string
			setup  func(t *testing.T, dir string) string
			reason Reason
		}{"url " + u, func(t *testing.T, dir string) string { return writeService(t, dir, u, testPassword, 0o600) }, ReasonURL})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := c.setup(t, t.TempDir())
			_, err := ReadService(path)
			var se *ServiceError
			if !errors.As(err, &se) {
				t.Fatalf("want *ServiceError, got %T %v", err, err)
			}
			if se.Reason != c.reason {
				t.Errorf("reason %q, want %q", se.Reason, c.reason)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error does not name the file: %q", err.Error())
			}
			noPassword(t, "error", err.Error())
			noPassword(t, "error %#v", fmt.Sprintf("%#v", err))
		})
	}
}

func TestReadServiceWrongOwner(t *testing.T) {
	path := writeService(t, t.TempDir(), "http://127.0.0.1:1", testPassword, 0o600)
	orig := currentUID
	currentUID = func() int { return orig() + 1 }
	t.Cleanup(func() { currentUID = orig })
	_, err := ReadService(path)
	var se *ServiceError
	if !errors.As(err, &se) || se.Reason != ReasonOwner {
		t.Fatalf("want owner refusal, got %v", err)
	}
	noPassword(t, "error", err.Error())
}

func TestServiceNeverPrintsPassword(t *testing.T) {
	path := writeService(t, t.TempDir(), "http://127.0.0.1:49374", testPassword, 0o600)
	s, err := ReadService(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{s, &s} {
		for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
			noPassword(t, f, fmt.Sprintf(f, v))
		}
		noPassword(t, "Sprint", fmt.Sprint(v))
	}
}

func TestDefaultServicePath(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/x/state")
	if got := DefaultServicePath(); got != "/x/state/opencode/service.json" {
		t.Errorf("got %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/h")
	if got := DefaultServicePath(); got != "/h/.local/state/opencode/service.json" {
		t.Errorf("got %q", got)
	}
}

func sprintf(f string, v any) string { return fmt.Sprintf(f, v) }
