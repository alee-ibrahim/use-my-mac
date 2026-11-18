package selfupdate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a release version "vMAJOR.MINOR.PATCH[-PRERELEASE]". Builds made
// with `git describe` ("v0.1.1-3-gabc1234[-dirty]") parse too: they sort just
// above their base tag and below the next release.
type Version struct {
	Major, Minor, Patch int
	Pre                 string // "" for a release
	Describe            int    // commits after the tag for git-describe builds, else 0
	raw                 string
}

var (
	versionRe  = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)
	describeRe = regexp.MustCompile(`^(\d+)-g[0-9a-f]{4,40}(?:-dirty)?$`)
)

// ParseVersion parses a version. A leading "v" is required: it is how tags,
// binaries and the router report versions.
func ParseVersion(s string) (Version, error) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("%q is not a release version (want vX.Y.Z)", s)
	}
	v := Version{raw: m[0]}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	if d := describeRe.FindStringSubmatch(m[4]); d != nil {
		v.Describe, _ = strconv.Atoi(d[1])
	} else {
		v.Pre = m[4]
	}
	return v, nil
}

// IsRelease reports whether s is a plain tagged version (not a git-describe build).
func IsRelease(s string) bool {
	v, err := ParseVersion(s)
	return err == nil && v.Describe == 0
}

func (v Version) String() string { return v.raw }

// Compare returns -1, 0 or 1. Pre-releases sort below their release
// (semver), identifiers compared numerically when both are numbers.
func Compare(a, b Version) int {
	for _, d := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			return cmpInt(d[0], d[1])
		}
	}
	if c := comparePre(a.Pre, b.Pre); c != 0 {
		return c
	}
	return cmpInt(a.Describe, b.Describe)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		switch {
		case ea == nil && eb == nil:
			if na != nb {
				return cmpInt(na, nb)
			}
		case ea == nil:
			return -1
		case eb == nil:
			return 1
		default:
			if c := strings.Compare(pa[i], pb[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(pa), len(pb))
}
