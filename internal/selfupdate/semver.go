// Package selfupdate replaces the running deploykit binary by the latest release of the project, and tells once a day when
// there is one. Nothing here runs by itself: the commands decide when it is used.
package selfupdate

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a semantic version (https://semver.org): 0.5.1, 1.2.3-rc.1. Build metadata after a + is ignored, as the
// specification says for precedence.
type Version struct {
	Major, Minor, Patch int
	Pre                 []string
}

// Parse reads a version with or without its leading v.
func Parse(s string) (Version, error) {
	orig := s
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%q is not a version (X.Y.Z)", orig)
	}
	var nums [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("%q is not a version (X.Y.Z)", orig)
		}
		nums[i] = n
	}
	v := Version{Major: nums[0], Minor: nums[1], Patch: nums[2]}
	if hasPre {
		v.Pre = strings.Split(pre, ".")
		for _, id := range v.Pre {
			if id == "" {
				return Version{}, fmt.Errorf("%q is not a version (empty pre-release field)", orig)
			}
		}
	}
	return v, nil
}

// String is the version without a leading v.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	return s
}

// Prerelease reports whether v is a release candidate and the like.
func (v Version) Prerelease() bool { return len(v.Pre) > 0 }

// Compare returns -1, 0 or 1 as v is older than, the same as, or newer than o. A pre-release is older than the release it leads to.
func (v Version) Compare(o Version) int {
	for _, d := range [3]int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		if d != 0 {
			return sign(d)
		}
	}
	switch {
	case len(v.Pre) == 0 && len(o.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(o.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(o.Pre); i++ {
		if c := comparePreID(v.Pre[i], o.Pre[i]); c != 0 {
			return c
		}
	}
	return sign(len(v.Pre) - len(o.Pre))
}

// comparePreID orders two pre-release identifiers: numbers by value, and before words; words by their letters.
func comparePreID(a, b string) int {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return sign(an - bn)
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	}
	return sign(strings.Compare(a, b))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
