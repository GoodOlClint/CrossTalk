package opencode

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
)

var ErrUntestedVersion = errors.New("opencode: service version outside the tested set")

// TestedMajors are the majors an agent may bind on; TestedMinors are the
// major.minor pairs verified live. Other minors of a tested major warn.
var (
	TestedMajors = []int{2}
	TestedMinors = []string{"2.4"}
)

var versionSyntax = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$`)

// CheckVersion refuses anything but a plain X.Y.Z version of a major in
// TestedMajors, and returns a warning for an untested minor. A refused value
// is never echoed.
func CheckVersion(v string) (warning string, err error) {
	m := versionSyntax.FindStringSubmatch(v)
	if m == nil {
		return "", fmt.Errorf("%w: not a plain major.minor.patch version", ErrUntestedVersion)
	}
	major, _ := strconv.Atoi(m[1])
	if !slices.Contains(TestedMajors, major) {
		return "", fmt.Errorf("%w: major %d (tested majors %v)", ErrUntestedVersion, major, TestedMajors)
	}
	if !slices.Contains(TestedMinors, m[1]+"."+m[2]) {
		return fmt.Sprintf("OpenCode %s is an untested minor (tested %v)", v, TestedMinors), nil
	}
	return "", nil
}
