// Package resolver parses npm package specs and picks the best matching
// version from registry metadata.
package resolver

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"

	"bale/internal/registry"
)

// ParseSpec splits a package spec such as "name", "name@1.2.3",
// "name@^1.0.0", "@scope/name", or "@scope/name@latest" into a package name
// and a version range. An empty range defaults to "latest".
func ParseSpec(spec string) (name, rng string, err error) {
	if spec == "" {
		return "", "", fmt.Errorf("empty package spec")
	}

	prefix := ""
	search := spec
	if strings.HasPrefix(spec, "@") {
		prefix = "@"
		search = spec[1:]
	}
	if search == "" {
		return "", "", fmt.Errorf("invalid package spec %q", spec)
	}

	if idx := strings.Index(search, "@"); idx >= 0 {
		name = prefix + search[:idx]
		rng = search[idx+1:]
	} else {
		name = prefix + search
	}

	if name == "" || name == "@" {
		return "", "", fmt.Errorf("invalid package spec %q", spec)
	}
	if rng == "" {
		rng = "latest"
	}
	return name, rng, nil
}

// Pick chooses the best version from the packument for rng.
//
// A rng that parses as a semver range is always resolved as a range and
// never as a dist-tag, matching npm: "pkg@1.0.0" must install 1.0.0 even if
// the registry also publishes a dist-tag literally named "1.0.0". Only a
// rng that is not a range is looked up in dist-tags.
//
// In both cases the version key in the packument's "versions" map is
// authoritative: a version whose own "version" field disagrees with its key
// is rejected rather than silently trusted.
func Pick(p *registry.Packument, rng string) (*registry.Version, error) {
	if !IsRange(rng) {
		pinned, ok := p.DistTags[rng]
		if !ok {
			return nil, fmt.Errorf("unknown dist-tag %q for %s", rng, p.Name)
		}
		v, ok := p.Versions[pinned]
		if !ok {
			return nil, fmt.Errorf("dist-tag %q of %s points to missing version %s", rng, p.Name, pinned)
		}
		if v.Version != pinned {
			return nil, fmt.Errorf("registry metadata for %s: version %q reports version %q", p.Name, pinned, v.Version)
		}
		return &v, nil
	}

	constraintStr := rng
	if constraintStr == "" {
		constraintStr = "*"
	}
	constraint, err := semver.NewConstraint(constraintStr)
	if err != nil {
		return nil, fmt.Errorf("invalid version range %q for %s: %w", rng, p.Name, err)
	}

	var best *semver.Version
	var bestKey string
	var skipped int
	for key := range p.Versions {
		sv, err := semver.NewVersion(key)
		if err != nil {
			skipped++ // skip versions that don't parse as semver
			continue
		}
		if !constraint.Check(sv) {
			continue
		}
		if best == nil || sv.GreaterThan(best) {
			best = sv
			bestKey = key
		}
	}
	if best == nil {
		msg := fmt.Sprintf("no version of %s satisfies %s", p.Name, rng)
		if skipped > 0 {
			msg += fmt.Sprintf(" (%d of %d published versions are not valid semver)", skipped, len(p.Versions))
		}
		return nil, errors.New(msg)
	}

	picked := p.Versions[bestKey]
	if picked.Version != bestKey {
		return nil, fmt.Errorf("registry metadata for %s: version %q reports version %q", p.Name, bestKey, picked.Version)
	}
	return &picked, nil
}

// Satisfies reports whether version satisfies rng. Dist-tag ranges such as
// "latest" are not semver constraints and always report false.
func Satisfies(version, rng string) bool {
	sv, err := semver.NewVersion(version)
	if err != nil {
		return false
	}
	constraintStr := rng
	if constraintStr == "" {
		constraintStr = "*"
	}
	constraint, err := semver.NewConstraint(constraintStr)
	if err != nil {
		return false
	}
	return constraint.Check(sv)
}

// IsRange reports whether rng parses as a semver range (as opposed to a
// dist-tag such as "latest"). Both "" and "*" count as ranges.
func IsRange(rng string) bool {
	constraintStr := rng
	if constraintStr == "" {
		constraintStr = "*"
	}
	_, err := semver.NewConstraint(constraintStr)
	return err == nil
}

// maxNameLength is npm's limit on package name length.
const maxNameLength = 214

// nameSegment matches one path segment of a package name: the whole name
// for unscoped packages, or the scope and the name for "@scope/name". The
// leading character must be alphanumeric, which rules out ".", "..", and
// hidden or underscore-prefixed names; the remaining characters are
// restricted to the URL-safe set npm has always allowed.
var nameSegment = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._~-]*$`)

// tagPattern matches a plausible dist-tag identifier such as "latest",
// "beta", or "next-2". It deliberately does not allow whitespace or
// range operators, so a malformed semver range is not mistaken for a tag.
var tagPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ValidateName reports whether name is an acceptable npm package name.
// Every name, whether typed on the command line or read from registry
// metadata, must pass this check before it is used to build a filesystem
// path or a registry URL. It rejects anything that could escape
// node_modules (".", "..", "/", absolute paths) as well as the reserved
// name "node_modules".
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("invalid package name: empty")
	}
	if len(name) > maxNameLength {
		return fmt.Errorf("invalid package name %q: longer than %d characters", name, maxNameLength)
	}

	segments := []string{name}
	if strings.HasPrefix(name, "@") {
		scope, rest, ok := strings.Cut(name[1:], "/")
		if !ok {
			return fmt.Errorf("invalid package name %q: scoped name must be @scope/name", name)
		}
		segments = []string{scope, rest}
	}
	for _, seg := range segments {
		if !nameSegment.MatchString(seg) {
			return fmt.Errorf("invalid package name %q", name)
		}
	}
	if strings.EqualFold(segments[len(segments)-1], "node_modules") {
		return fmt.Errorf("invalid package name %q: reserved", name)
	}
	return nil
}

// ValidateRange reports whether rng is either a semver range or a
// plausible dist-tag. Anything else (for example a range with a typo in
// it) is an error, so that a bad range fails the same way regardless of
// what happens to be installed already.
func ValidateRange(rng string) error {
	if IsRange(rng) || tagPattern.MatchString(rng) {
		return nil
	}
	return fmt.Errorf("invalid version range %q", rng)
}
