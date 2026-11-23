package opencode

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

var ErrUntestedVersion = errors.New("opencode: service version outside the tested set")

// TestedMajors are the majors an agent may bind on; TestedMinors are the
// major.minor pairs verified live. Other minors of a tested major warn.
var (
	TestedMajors = []int{2}
	TestedMinors = []string{"2.4"}
)

// CheckVersion refuses an unparseable version or a major outside
// TestedMajors, and returns a warning for an untested minor.
func CheckVersion(v string) (warning string, err error) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("%w: unparseable %q", ErrUntestedVersion, v)
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return "", fmt.Errorf("%w: unparseable %q", ErrUntestedVersion, v)
	}
	if !slices.Contains(TestedMajors, major) {
		return "", fmt.Errorf("%w: %s (tested majors %v)", ErrUntestedVersion, v, TestedMajors)
	}
	if mm := fmt.Sprintf("%d.%d", major, minor); !slices.Contains(TestedMinors, mm) {
		return fmt.Sprintf("OpenCode %s is an untested minor (tested %v)", v, TestedMinors), nil
	}
	return "", nil
}
