package resolver

import (
	"strings"
	"testing"

	"bale/internal/registry"
)

func TestParseSpec(t *testing.T) {
	tests := []struct {
		spec    string
		name    string
		rng     string
		wantErr bool
	}{
		{spec: "foo", name: "foo", rng: "latest"},
		{spec: "foo@1.2.3", name: "foo", rng: "1.2.3"},
		{spec: "foo@^1.0.0", name: "foo", rng: "^1.0.0"},
		{spec: "foo@latest", name: "foo", rng: "latest"},
		{spec: "@scope/name", name: "@scope/name", rng: "latest"},
		{spec: "@scope/name@latest", name: "@scope/name", rng: "latest"},
		{spec: "@scope/name@^2.0.0", name: "@scope/name", rng: "^2.0.0"},
		{spec: "", wantErr: true},
		{spec: "@", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			name, rng, err := ParseSpec(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseSpec(%q) = nil error, want error", tt.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSpec(%q) unexpected error: %v", tt.spec, err)
			}
			if name != tt.name || rng != tt.rng {
				t.Errorf("ParseSpec(%q) = (%q, %q), want (%q, %q)", tt.spec, name, rng, tt.name, tt.rng)
			}
		})
	}
}

func testPackument() *registry.Packument {
	return &registry.Packument{
		Name: "pkg",
		DistTags: map[string]string{
			"latest": "1.5.0",
		},
		Versions: map[string]registry.Version{
			"1.0.0": {Name: "pkg", Version: "1.0.0"},
			"1.2.0": {Name: "pkg", Version: "1.2.0"},
			"1.5.0": {Name: "pkg", Version: "1.5.0"},
			"2.0.0": {Name: "pkg", Version: "2.0.0"},
		},
	}
}

func TestPick(t *testing.T) {
	p := testPackument()

	tests := []struct {
		name    string
		rng     string
		want    string
		wantErr bool
	}{
		{name: "caret", rng: "^1.0.0", want: "1.5.0"},
		{name: "tilde", rng: "~1.2.0", want: "1.2.0"},
		{name: "exact", rng: "1.2.0", want: "1.2.0"},
		{name: "star", rng: "*", want: "2.0.0"},
		{name: "empty", rng: "", want: "2.0.0"},
		{name: "dist-tag", rng: "latest", want: "1.5.0"},
		{name: "no-match", rng: "^9.0.0", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := Pick(p, tt.rng)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Pick(%q) = nil error, want error", tt.rng)
				}
				return
			}
			if err != nil {
				t.Fatalf("Pick(%q) unexpected error: %v", tt.rng, err)
			}
			if v.Version != tt.want {
				t.Errorf("Pick(%q) = %q, want %q", tt.rng, v.Version, tt.want)
			}
		})
	}
}

func TestPickSkipsUnparsableVersions(t *testing.T) {
	p := &registry.Packument{
		Name: "pkg",
		Versions: map[string]registry.Version{
			"not-semver": {Name: "pkg", Version: "not-semver"},
			"1.0.0":      {Name: "pkg", Version: "1.0.0"},
		},
	}
	v, err := Pick(p, "*")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if v.Version != "1.0.0" {
		t.Errorf("Pick = %q, want 1.0.0", v.Version)
	}
}

func TestIsRange(t *testing.T) {
	tests := []struct {
		rng  string
		want bool
	}{
		{"", true},
		{"*", true},
		{"^1.0.0", true},
		{"~1.0.0", true},
		{"1.2.3", true},
		{">=1.0.0 <2.0.0", true},
		{"latest", false},
		{"beta", false},
		{"next", false},
	}
	for _, tt := range tests {
		got := IsRange(tt.rng)
		if got != tt.want {
			t.Errorf("IsRange(%q) = %v, want %v", tt.rng, got, tt.want)
		}
	}
}

func TestSatisfies(t *testing.T) {
	tests := []struct {
		version string
		rng     string
		want    bool
	}{
		{"1.5.0", "^1.0.0", true},
		{"2.0.0", "^1.0.0", false},
		{"1.5.0", "latest", false}, // dist-tag ranges are never semver constraints
		{"not-semver", "^1.0.0", false},
	}
	for _, tt := range tests {
		got := Satisfies(tt.version, tt.rng)
		if got != tt.want {
			t.Errorf("Satisfies(%q, %q) = %v, want %v", tt.version, tt.rng, got, tt.want)
		}
	}
}

func TestValidateName(t *testing.T) {
	valid := []string{
		"lodash", "is-odd", "underscore.string", "JSONStream", "a~b", "x",
		"@babel/core", "@scope/name.with.dots", "@Scope/Name",
	}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"", ".", "..", "../x", "../../victim", "./x", "/abs", "a/b", "a//b",
		"@scope", "@scope/", "@/name", "@scope/../x", "@../x/y", "@scope/./x",
		".hidden", "_private", "a b", "a\\b", "a\x00b", "node_modules",
		"NODE_MODULES", "@scope/node_modules", "a\nb", "a:b",
		strings.Repeat("a", 215),
	}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", name)
		}
	}
}

func TestValidateRange(t *testing.T) {
	valid := []string{"", "*", "1.2.3", "^1.0.0", "~1.2", ">=1.0.0 <2.0.0", "1.x", "latest", "beta", "next-2", "v1"}
	for _, rng := range valid {
		if err := ValidateRange(rng); err != nil {
			t.Errorf("ValidateRange(%q) = %v, want nil", rng, err)
		}
	}
	invalid := []string{"totally bogus range", "^^1.0.0", "1.0.0 ||| 2.0.0", "la test", "^1.0.0 and"}
	for _, rng := range invalid {
		if err := ValidateRange(rng); err == nil {
			t.Errorf("ValidateRange(%q) = nil, want error", rng)
		}
	}
}

// TestPickPrefersRangeOverDistTag pins npm semantics: an exact version spec
// must resolve as a semver range even when a dist-tag has the same name.
func TestPickPrefersRangeOverDistTag(t *testing.T) {
	p := &registry.Packument{
		Name:     "pkg",
		DistTags: map[string]string{"1.0.0": "2.0.0", "latest": "2.0.0"},
		Versions: map[string]registry.Version{
			"1.0.0": {Name: "pkg", Version: "1.0.0"},
			"2.0.0": {Name: "pkg", Version: "2.0.0"},
		},
	}
	v, err := Pick(p, "1.0.0")
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if v.Version != "1.0.0" {
		t.Errorf("Pick(%q) = %q, want 1.0.0 (dist-tag must not win)", "1.0.0", v.Version)
	}
}

func TestPickUnknownDistTag(t *testing.T) {
	p := testPackument()
	_, err := Pick(p, "nightly")
	if err == nil {
		t.Fatal("expected error for unknown dist-tag")
	}
	if !strings.Contains(err.Error(), `unknown dist-tag "nightly" for pkg`) {
		t.Errorf("error = %q, want unknown dist-tag", err)
	}
}

func TestPickDistTagPointsToMissingVersion(t *testing.T) {
	p := testPackument()
	p.DistTags["beta"] = "9.9.9"
	_, err := Pick(p, "beta")
	if err == nil {
		t.Fatal("expected error for dangling dist-tag")
	}
	if !strings.Contains(err.Error(), "points to missing version 9.9.9") {
		t.Errorf("error = %q", err)
	}
}

func TestPickRejectsVersionIdentityMismatch(t *testing.T) {
	t.Run("range path", func(t *testing.T) {
		p := &registry.Packument{
			Name: "pkg",
			Versions: map[string]registry.Version{
				"1.0.0": {Name: "pkg", Version: "1.0.1"},
			},
		}
		_, err := Pick(p, "^1.0.0")
		if err == nil {
			t.Fatal("expected error for mismatched version identity")
		}
		if !strings.Contains(err.Error(), `registry metadata for pkg: version "1.0.0" reports version "1.0.1"`) {
			t.Errorf("error = %q", err)
		}
	})

	t.Run("dist-tag path", func(t *testing.T) {
		p := &registry.Packument{
			Name:     "pkg",
			DistTags: map[string]string{"latest": "1.0.0"},
			Versions: map[string]registry.Version{
				"1.0.0": {Name: "pkg", Version: ""},
			},
		}
		_, err := Pick(p, "latest")
		if err == nil {
			t.Fatal("expected error for mismatched version identity")
		}
		if !strings.Contains(err.Error(), `registry metadata for pkg: version "1.0.0" reports version ""`) {
			t.Errorf("error = %q", err)
		}
	})
}

func TestPickNoMatchReportsSkippedVersions(t *testing.T) {
	p := &registry.Packument{
		Name: "pkg",
		Versions: map[string]registry.Version{
			"1.0.0":      {Name: "pkg", Version: "1.0.0"},
			"not-semver": {Name: "pkg", Version: "not-semver"},
			"also-bad":   {Name: "pkg", Version: "also-bad"},
		},
	}
	_, err := Pick(p, "^9.0.0")
	if err == nil {
		t.Fatal("expected error for unsatisfiable range")
	}
	want := "no version of pkg satisfies ^9.0.0 (2 of 3 published versions are not valid semver)"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestPickNoMatchOmitsSkippedCountWhenZero(t *testing.T) {
	_, err := Pick(testPackument(), "^9.0.0")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got != "no version of pkg satisfies ^9.0.0" {
		t.Errorf("error = %q, want no parenthetical", got)
	}
}
