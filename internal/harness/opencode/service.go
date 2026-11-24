package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

type Reason string

const (
	ReasonMissing    Reason = "file is missing"
	ReasonSymlink    Reason = "file is a symlink"
	ReasonNotRegular Reason = "not a regular file"
	ReasonMode       Reason = "mode is not 0600"
	ReasonOwner      Reason = "not owned by the current user"
	ReasonJSON       Reason = "not valid JSON of the expected shape"
	ReasonURL        Reason = "url is not a canonical loopback http origin"
	ReasonPassword   Reason = "password is empty"
	ReasonRead       Reason = "cannot be read"
)

type ServiceError struct {
	File   string
	Reason Reason
}

func (e *ServiceError) Error() string {
	return fmt.Sprintf("opencode service file %s: %s", e.File, e.Reason)
}

const maxServiceFile = 1 << 16

var currentUID = os.Geteuid

type Service struct {
	Origin   string
	PID      int
	Version  string
	password string
}

func (s Service) Format(f fmt.State, _ rune) {
	fmt.Fprintf(f, "opencode.Service{Origin:%s PID:%d Version:%s}", s.Origin, s.PID, s.Version)
}

func DefaultServicePath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(base) {
		return ""
	}
	return filepath.Join(base, "opencode", "service.json")
}

func ReadService(path string) (Service, error) {
	fail := func(r Reason) (Service, error) { return Service{}, &ServiceError{File: path, Reason: r} }
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fail(ReasonMissing)
	case errors.Is(err, syscall.ELOOP):
		return fail(ReasonSymlink)
	case err != nil:
		return fail(ReasonRead)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fail(ReasonRead)
	}
	if !fi.Mode().IsRegular() {
		return fail(ReasonNotRegular)
	}
	if fi.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0o600 {
		return fail(ReasonMode)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != currentUID() {
		return fail(ReasonOwner)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxServiceFile+1))
	if err != nil || len(raw) > maxServiceFile {
		return fail(ReasonRead)
	}
	var doc struct {
		Version  string `json:"version"`
		URL      string `json:"url"`
		PID      int    `json:"pid"`
		Password string `json:"password"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return fail(ReasonJSON)
	}
	origin, ok := loopbackOrigin(doc.URL)
	if !ok {
		return fail(ReasonURL)
	}
	if doc.PID <= 0 {
		return fail(ReasonJSON)
	}
	if doc.Password == "" {
		return fail(ReasonPassword)
	}
	return Service{Origin: origin, PID: doc.PID, Version: doc.Version, password: doc.Password}, nil
}

func loopbackOrigin(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Opaque != "" {
		return "", false
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "::1" {
		return "", false
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != u.Port() {
		return "", false
	}
	return "http://" + net.JoinHostPort(host, u.Port()), true
}
