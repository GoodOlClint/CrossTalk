package codex

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var ErrUntestedVersion = errors.New("codex: app-server version outside the tested set")

// TestedMajors are the majors an agent may bind on; TestedMinors are the
// major.minor pairs verified live (V11). Other minors of a tested major warn.
var (
	TestedMajors = []int{0}
	TestedMinors = []string{"0.166"}
)

// CheckVersion refuses an unparseable version or a major outside
// TestedMajors, and reports warn for an untested minor.
func CheckVersion(v string) (warn bool, err error) {
	parts := strings.SplitN(v, ".", 3)
	if v == "" || len(parts) < 2 {
		return false, fmt.Errorf("%w: unparseable %q", ErrUntestedVersion, v)
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false, fmt.Errorf("%w: unparseable %q", ErrUntestedVersion, v)
	}
	tested := false
	for _, m := range TestedMajors {
		tested = tested || m == major
	}
	if !tested {
		return false, fmt.Errorf("%w: %s (tested majors %v)", ErrUntestedVersion, v, TestedMajors)
	}
	mm := fmt.Sprintf("%d.%d", major, minor)
	for _, t := range TestedMinors {
		if t == mm {
			return false, nil
		}
	}
	return true, nil
}
