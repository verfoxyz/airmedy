package pluginhost

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/blang/semver"

	"airmedy/internal/domain"
)

// API version negotiation.
//
// Manifests declare the API range they accept (`"apiVersion": "^1.0"`), which
// is npm-flavoured semver. A plugin answers the handshake with one concrete
// version (`"apiVersion": "1.0"`). Both are checked:
//
//  1. the concrete version must satisfy the manifest's own range, so a
//     mislabelled plugin is rejected instead of running under a claim it does
//     not honour; and
//  2. the concrete version must be speakable by this host: same major, and not
//     a newer minor (1.x only ever adds, see domain.HostAPIVersion).
//
// blang/semver is the repo's existing semver dependency but its range parser
// only accepts strict comparators, so npm shorthands are expanded first.

// parseAPIRange parses a manifest-declared API version range.
func parseAPIRange(raw string) (semver.Range, error) {
	normalized, err := normalizeAPIRange(raw)
	if err != nil {
		return nil, err
	}
	rng, err := semver.ParseRange(normalized)
	if err != nil {
		return nil, fmt.Errorf("%w: apiVersion %q is not a valid version range: %v",
			domain.ErrPluginManifestInvalid, raw, err)
	}
	return rng, nil
}

// apiVersionSatisfies reports whether a concrete plugin API version matches a
// manifest-declared range.
func apiVersionSatisfies(rangeRaw, versionRaw string) (bool, error) {
	rng, err := parseAPIRange(rangeRaw)
	if err != nil {
		return false, err
	}
	version, err := semver.ParseTolerant(strings.TrimSpace(versionRaw))
	if err != nil {
		return false, fmt.Errorf("%w: apiVersion %q is not a valid version: %v",
			domain.ErrPluginManifestInvalid, versionRaw, err)
	}
	return rng(version), nil
}

// apiVersionCompatible reports whether this host can speak a plugin's concrete
// API version. The host promises additive-only changes inside a major, so an
// older minor is fine and a newer one is not.
func apiVersionCompatible(pluginVersion, hostVersion string) bool {
	plugin, err := semver.ParseTolerant(strings.TrimSpace(pluginVersion))
	if err != nil {
		return false
	}
	host, err := semver.ParseTolerant(strings.TrimSpace(hostVersion))
	if err != nil {
		return false
	}
	return plugin.Major == host.Major && plugin.Minor <= host.Minor
}

// normalizeAPIRange rewrites npm-style range shorthands into the strict
// comparator syntax blang/semver accepts.
func normalizeAPIRange(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: apiVersion is empty", domain.ErrPluginManifestInvalid)
	}
	// Only conjunctions (space-separated comparators) are supported: the
	// underlying range parser mishandles `||`, and an apiVersion range never
	// needs a disjunction in practice.
	return normalizeConjunction(trimmed)
}

// normalizeConjunction expands one space-separated (AND) group of comparators.
func normalizeConjunction(raw string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 {
		return "", fmt.Errorf("%w: apiVersion range %q is empty", domain.ErrPluginManifestInvalid, raw)
	}
	expanded := make([]string, 0, len(fields)*2)
	for _, field := range fields {
		comparators, err := expandRangeToken(field)
		if err != nil {
			return "", err
		}
		expanded = append(expanded, comparators...)
	}
	return strings.Join(expanded, " "), nil
}

// expandRangeToken expands one comparator or shorthand into strict comparators.
func expandRangeToken(token string) ([]string, error) {
	if token == "*" || strings.EqualFold(token, "x") {
		return []string{">=0.0.0"}, nil
	}
	if strings.HasPrefix(token, "^") {
		return expandCaret(strings.TrimPrefix(token, "^"))
	}
	if strings.HasPrefix(token, "~") {
		return expandTilde(strings.TrimPrefix(token, "~"))
	}

	operator := ""
	for _, candidate := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(token, candidate) {
			operator = candidate
			token = strings.TrimSpace(token[len(candidate):])
			break
		}
	}
	if operator == "=" && (token == "*" || strings.EqualFold(token, "x")) {
		return []string{">=0.0.0"}, nil
	}

	spec, err := parseVersionSpec(token)
	if err != nil {
		return nil, fmt.Errorf("%w: apiVersion comparator %q: %v",
			domain.ErrPluginManifestInvalid, token, err)
	}
	if spec.wildcardAt >= 0 {
		return wildcardComparators(spec), nil
	}

	switch operator {
	case "":
		return bareVersionComparators(spec), nil
	case "=":
		if spec.parts < 3 {
			return bareVersionComparators(spec), nil
		}
		return []string{"=" + spec.version.String()}, nil
	default:
		return []string{operator + spec.version.String()}, nil
	}
}

// expandCaret turns `^1.2.3` into `>=1.2.3 <2.0.0`. For 0.x the upper bound
// moves the first non-zero component, as npm does.
func expandCaret(raw string) ([]string, error) {
	spec, err := parseVersionSpec(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: apiVersion comparator ^%s: %v",
			domain.ErrPluginManifestInvalid, raw, err)
	}
	if spec.wildcardAt >= 0 {
		return wildcardComparators(spec), nil
	}
	upper := semver.Version{Major: spec.version.Major + 1}
	if spec.version.Major == 0 {
		if spec.version.Minor > 0 {
			upper = semver.Version{Minor: spec.version.Minor + 1}
		} else {
			upper = semver.Version{Patch: spec.version.Patch + 1}
		}
	}
	return []string{">=" + spec.version.String(), "<" + upper.String()}, nil
}

// expandTilde turns `~1.2.3` into `>=1.2.3 <1.3.0`; a bare `~1` stays within
// major 1.
func expandTilde(raw string) ([]string, error) {
	spec, err := parseVersionSpec(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: apiVersion comparator ~%s: %v",
			domain.ErrPluginManifestInvalid, raw, err)
	}
	if spec.wildcardAt >= 0 {
		return wildcardComparators(spec), nil
	}
	upper := semver.Version{Major: spec.version.Major + 1}
	if spec.parts >= 2 {
		upper = semver.Version{Major: spec.version.Major, Minor: spec.version.Minor + 1}
	}
	return []string{">=" + spec.version.String(), "<" + upper.String()}, nil
}

// bareVersionComparators gives a version written without an operator a range
// that matches its declared granularity: `1` means any 1.x, `1.2` any 1.2.x,
// `1.2.3` exactly that version.
func bareVersionComparators(spec versionSpec) []string {
	switch spec.parts {
	case 1:
		return []string{
			">=" + spec.version.String(),
			fmt.Sprintf("<%d.0.0", spec.version.Major+1),
		}
	case 2:
		return []string{
			">=" + spec.version.String(),
			fmt.Sprintf("<%d.%d.0", spec.version.Major, spec.version.Minor+1),
		}
	default:
		return []string{"=" + spec.version.String()}
	}
}

// wildcardComparators handles `1.x` / `1.2.x` / `*`. The position of the
// wildcard decides how wide the range is, not how many components were written.
func wildcardComparators(spec versionSpec) []string {
	switch spec.wildcardAt {
	case 0:
		return []string{">=0.0.0"}
	case 1:
		return []string{
			">=" + spec.version.String(),
			fmt.Sprintf("<%d.0.0", spec.version.Major+1),
		}
	default:
		return []string{
			">=" + spec.version.String(),
			fmt.Sprintf("<%d.%d.0", spec.version.Major, spec.version.Minor+1),
		}
	}
}

// versionSpec is a partially written semantic version plus how much of it the
// author wrote, which decides how a bare version is widened into a range.
type versionSpec struct {
	version semver.Version
	parts   int
	// wildcardAt is the index of the first `x`/`*` component, or -1 when the
	// version is fully specified.
	wildcardAt int
}

// parseVersionSpec parses `1`, `1.2`, `1.2.3`, `1.x`, `1.2.x` and the
// prerelease forms semver.ParseTolerant accepts when all three parts are
// written out.
func parseVersionSpec(raw string) (versionSpec, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	if trimmed == "" {
		return versionSpec{}, fmt.Errorf("version is empty")
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) > 3 {
		return versionSpec{}, fmt.Errorf("version %q has more than three components", raw)
	}

	var numbers [3]uint64
	wildcardAt := -1
	for i, part := range parts {
		switch strings.ToLower(part) {
		case "x", "*", "":
			if wildcardAt < 0 {
				wildcardAt = i
			}
			continue
		}
		number, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			// A prerelease/build suffix (1.2.3-beta.1) is only meaningful when
			// the version is fully written; let semver judge it.
			if len(parts) == 3 {
				version, parseErr := semver.ParseTolerant(trimmed)
				if parseErr != nil {
					return versionSpec{}, fmt.Errorf("version %q: %v", raw, parseErr)
				}
				return versionSpec{version: version, parts: 3}, nil
			}
			return versionSpec{}, fmt.Errorf("version %q: %q is not a number", raw, part)
		}
		numbers[i] = number
	}
	return versionSpec{
		version:    semver.Version{Major: numbers[0], Minor: numbers[1], Patch: numbers[2]},
		parts:      len(parts),
		wildcardAt: wildcardAt,
	}, nil
}
