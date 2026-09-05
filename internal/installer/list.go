package installer

import (
	"errors"
	"fmt"
	"io/fs"
	"text/tabwriter"

	"github.com/Masterminds/semver/v3"

	"bale/internal/manifest"
	"bale/internal/resolver"
)

// List reports direct dependencies using local manifests only. It never
// contacts the registry or changes files. Invalid or missing packages are
// displayed along with healthy ones, then reported through a non-nil error.
func (in *Installer) List() error {
	m, err := manifest.Load(in.manifestPath())
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no package.json found in %s", in.Dir)
	}
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	deps, err := depReqs("package.json", m.Dependencies)
	if err != nil {
		return err
	}
	devDeps, err := depReqs("package.json", m.DevDependencies)
	if err != nil {
		return err
	}
	if len(deps)+len(devDeps) == 0 {
		_, err := fmt.Fprintln(in.out(), "No dependencies declared.")
		return err
	}

	w := tabwriter.NewWriter(in.out(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "PACKAGE\tINSTALLED\tREQUESTED\tTYPE\tSTATUS")
	problems := 0
	for _, group := range []struct {
		kind string
		reqs []req
	}{
		{"prod", deps},
		{"dev", devDeps},
	} {
		for _, r := range group.reqs {
			version, status, problem := in.dependencyStatus(r)
			if problem {
				problems++
			}
			rng := r.rng
			if rng == "" {
				rng = "*"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.name, version, rng, group.kind, status)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("list: %w", err)
	}
	if problems > 0 {
		label := "problems"
		if problems == 1 {
			label = "problem"
		}
		return fmt.Errorf("found %d dependency %s", problems, label)
	}
	return nil
}

func (in *Installer) dependencyStatus(r req) (version, status string, problem bool) {
	m, err := in.preinstalled(r.name)
	if err != nil {
		return "-", "invalid package.json", true
	}
	version = "-"
	if m != nil {
		version = m.Version
	}
	if isUnsupportedSpec(r.rng) {
		return version, "unsupported spec", true
	}
	if err := resolver.ValidateRange(r.rng); err != nil {
		return version, "invalid range", true
	}
	if m == nil {
		return version, "missing", true
	}
	if m.Name != r.name {
		return version, "package name mismatch", true
	}
	if _, err := semver.NewVersion(version); err != nil {
		return version, "invalid version", true
	}
	if !resolver.IsRange(r.rng) {
		return version, "tag not checked (offline)", false
	}
	if !resolver.Satisfies(version, r.rng) {
		return version, "version mismatch", true
	}
	return version, "ok", false
}
