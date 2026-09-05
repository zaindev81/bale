// Package installer orchestrates resolving and installing npm packages into
// a flat node_modules directory.
package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"bale/internal/manifest"
	"bale/internal/registry"
	"bale/internal/resolver"
	"bale/internal/tarball"
)

// Installer resolves and installs packages for a single project.
type Installer struct {
	Dir      string // project directory
	Registry *registry.Client
	Out      io.Writer // progress output; defaults to os.Stdout
	ErrOut   io.Writer // warnings; defaults to os.Stderr
}

// New returns an Installer for the project at dir using reg as the registry
// client. Progress is written to os.Stdout and warnings to os.Stderr unless
// Out and ErrOut are set afterward.
func New(dir string, reg *registry.Client) *Installer {
	return &Installer{Dir: dir, Registry: reg, Out: os.Stdout, ErrOut: os.Stderr}
}

// req is a single pending dependency requirement: install name at rng.
type req struct {
	name   string
	rng    string
	direct bool // declared by the project rather than another package
}

// step is one resolved package to download and extract.
type step struct {
	name string
	ver  *registry.Version
}

// plan is the complete result of the resolution phase: every package that
// must be downloaded, in install order, plus the version decided for each
// name and which top-level names should be saved as "^<version>".
type plan struct {
	steps     []step
	installed map[string]string
	useCaret  map[string]bool
}

func (in *Installer) out() io.Writer {
	if in.Out != nil {
		return in.Out
	}
	return os.Stdout
}

func (in *Installer) errOut() io.Writer {
	if in.ErrOut != nil {
		return in.ErrOut
	}
	return os.Stderr
}

func (in *Installer) manifestPath() string {
	return filepath.Join(in.Dir, "package.json")
}

func (in *Installer) nodeModulesDir() string {
	return filepath.Join(in.Dir, "node_modules")
}

// Add resolves and installs each spec, then records the resulting range in
// package.json and saves it. The saved range is the user's explicit range
// when they gave one; a bare name or a dist-tag such as "latest" is recorded
// as "^<resolved version>". A package already listed in devDependencies is
// updated there rather than moved to dependencies.
func (in *Installer) Add(ctx context.Context, specs []string) error {
	m, err := manifest.Load(in.manifestPath())
	if errors.Is(err, fs.ErrNotExist) {
		// No package.json yet: build one in memory and create it on save.
		m, err = &manifest.Manifest{}, nil
	}
	if err != nil {
		return fmt.Errorf("add dependencies: %w", err)
	}

	queue := make([]req, 0, len(specs))
	seen := make(map[string]req, len(specs))
	order := make([]string, 0, len(specs))
	for _, spec := range specs {
		name, rng, err := resolver.ParseSpec(spec)
		if err != nil {
			return fmt.Errorf("add dependencies: %w", err)
		}
		if err := resolver.ValidateName(name); err != nil {
			return fmt.Errorf("add dependencies: %w", err)
		}
		queue = append(queue, req{name: name, rng: rng, direct: true})
		if _, dup := seen[name]; !dup {
			seen[name] = req{name: name, rng: rng}
			order = append(order, name)
		}
	}

	topLevel := make(map[string]bool, len(seen))
	for name := range seen {
		topLevel[name] = true
	}

	pl, err := in.install(ctx, queue, topLevel)
	if err != nil {
		return err
	}

	for _, name := range order {
		version, ok := pl.installed[name]
		if !ok {
			return fmt.Errorf("internal error: %s was requested but never resolved", name)
		}
		saveRange := seen[name].rng
		if pl.useCaret[name] {
			saveRange = "^" + version
		}
		if _, isDev := m.DevDependencies[name]; isDev {
			m.DevDependencies[name] = saveRange
		} else {
			m.AddDependency(name, saveRange)
		}
	}

	if err := m.Save(in.manifestPath()); err != nil {
		return fmt.Errorf("add dependencies: %w", err)
	}
	return nil
}

// InstallAll installs everything listed in package.json dependencies and
// devDependencies, including transitive dependencies missing from an
// incomplete node_modules tree. Direct dependencies are re-resolved when
// their installed versions do not satisfy the declared semver ranges.
func (in *Installer) InstallAll(ctx context.Context) error {
	m, err := manifest.Load(in.manifestPath())
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no package.json found in %s", in.Dir)
	}
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}

	queue, err := depReqs("package.json", m.Dependencies)
	if err != nil {
		return err
	}
	devQueue, err := depReqs("package.json", m.DevDependencies)
	if err != nil {
		return err
	}
	queue = append(queue, devQueue...)
	for i := range queue {
		queue[i].direct = true
	}

	if _, err := in.install(ctx, queue, nil); err != nil {
		return err
	}
	return nil
}

// depReqs turns a dependencies map into a queue of requirements sorted by
// name for deterministic install order, rejecting any name that is not a
// valid npm package name. declaredBy names the source of the map and is
// used in the error message.
func depReqs(declaredBy string, deps map[string]string) ([]req, error) {
	names := make([]string, 0, len(deps))
	for name := range deps {
		names = append(names, name)
	}
	sort.Strings(names)

	reqs := make([]req, 0, len(names))
	for _, name := range names {
		if err := resolver.ValidateName(name); err != nil {
			return nil, fmt.Errorf("%s declares invalid dependency name %q", declaredBy, name)
		}
		reqs = append(reqs, req{name: name, rng: deps[name]})
	}
	return reqs, nil
}

// install resolves queue completely, then applies the resulting plan. Both
// phases are separate on purpose: nothing is written to disk until every
// package in the tree has been resolved, so a missing package fails the
// install before it leaves a half-populated node_modules behind.
func (in *Installer) install(ctx context.Context, queue []req, topLevel map[string]bool) (*plan, error) {
	pl, err := in.resolve(ctx, queue, topLevel)
	if err != nil {
		return nil, err
	}
	if err := in.apply(ctx, pl); err != nil {
		return nil, err
	}
	return pl, nil
}

// resolve walks queue as a flat dependency resolution, contacting the
// registry for metadata only. Top-level requests (per topLevel) are always
// re-resolved and reinstalled, like `npm install foo` does; transitive
// requirements reuse whatever is already present in node_modules,
// first-wins, and their own dependencies are queued too so that an
// interrupted or hand-deleted tree is completed. Direct dependencies from
// InstallAll are reused only if compatible with their declared ranges.
func (in *Installer) resolve(ctx context.Context, queue []req, topLevel map[string]bool) (*plan, error) {
	pl := &plan{
		installed: make(map[string]string),
		useCaret:  make(map[string]bool),
	}

	for len(queue) > 0 {
		r := queue[0]
		queue = queue[1:]

		if isUnsupportedSpec(r.rng) {
			return nil, fmt.Errorf("unsupported dependency spec %q for %s", r.rng, r.name)
		}
		if err := resolver.ValidateRange(r.rng); err != nil {
			return nil, fmt.Errorf("dependency %s: %w", r.name, err)
		}

		if v, ok := pl.installed[r.name]; ok {
			in.warnIfIncompatible(r.name, v, r.rng)
			continue
		}

		// Transitive requirements reuse whatever is already in
		// node_modules, if present, even if it doesn't satisfy this
		// particular range (only a warning is emitted). Top-level
		// requests from Add skip this and are always resolved fresh below.
		// InstallAll repairs direct dependencies that no longer satisfy
		// package.json instead of keeping an incompatible version.
		if !topLevel[r.name] {
			m, err := in.preinstalled(r.name)
			if err != nil {
				fmt.Fprintf(in.errOut(), "warn: node_modules/%s/package.json is unreadable (%v); reinstalling\n", r.name, err)
			} else if m != nil && (!r.direct || compatible(m.Version, r.rng)) {
				pl.installed[r.name] = m.Version
				in.warnIfIncompatible(r.name, m.Version, r.rng)
				reqs, err := depReqs(fmt.Sprintf("package %s@%s", r.name, m.Version), m.Dependencies)
				if err != nil {
					return nil, err
				}
				queue = append(queue, reqs...)
				continue
			}
		}

		pack, err := in.Registry.Packument(ctx, r.name)
		if err != nil {
			return nil, err
		}
		if pack.Name != r.name {
			return nil, fmt.Errorf("registry returned package %q for request %q", pack.Name, r.name)
		}
		ver, err := resolver.Pick(pack, r.rng)
		if err != nil {
			return nil, err
		}

		pl.installed[r.name] = ver.Version
		if topLevel[r.name] {
			pl.useCaret[r.name] = !resolver.IsRange(r.rng)
		}
		pl.steps = append(pl.steps, step{name: r.name, ver: ver})

		reqs, err := depReqs(fmt.Sprintf("package %s@%s", r.name, ver.Version), ver.Dependencies)
		if err != nil {
			return nil, err
		}
		queue = append(queue, reqs...)
	}

	return pl, nil
}

// apply downloads and extracts every step of pl, in order.
func (in *Installer) apply(ctx context.Context, pl *plan) error {
	for i, s := range pl.steps {
		if err := in.installStep(ctx, s); err != nil {
			return fmt.Errorf("installed %d of %d packages before failing: %w", i, len(pl.steps), err)
		}
	}
	return nil
}

// installStep downloads one resolved version and swaps it into
// node_modules atomically: the tarball is unpacked into a temporary
// directory inside node_modules and verified there, and only a complete,
// correct package is renamed over the target.
func (in *Installer) installStep(ctx context.Context, s step) error {
	target, err := in.targetPath(s.name)
	if err != nil {
		return err
	}

	body, err := in.Registry.Download(ctx, s.ver.Dist)
	if err != nil {
		return err
	}

	nodeModules := in.nodeModulesDir()
	if err := os.MkdirAll(nodeModules, 0755); err != nil {
		return fmt.Errorf("install %s: %w", s.name, err)
	}
	tmp, err := os.MkdirTemp(nodeModules, ".bale-tmp-*")
	if err != nil {
		return fmt.Errorf("install %s: %w", s.name, err)
	}
	// A successful rename moves tmp away, making this a no-op.
	defer func() { _ = os.RemoveAll(tmp) }()

	if err := tarball.Extract(bytes.NewReader(body), tmp); err != nil {
		return fmt.Errorf("install %s: %w", s.name, err)
	}

	m, err := manifest.Load(filepath.Join(tmp, "package.json"))
	if err != nil {
		return fmt.Errorf("install %s: %w", s.name, err)
	}
	if m.Version != s.ver.Version {
		return fmt.Errorf("package %s: tarball reports version %q, expected %q", s.name, m.Version, s.ver.Version)
	}

	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("install %s: %w", s.name, err)
	}
	// Scoped packages live under node_modules/@scope/.
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("install %s: %w", s.name, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("install %s: %w", s.name, err)
	}

	fmt.Fprintf(in.out(), "+ %s@%s\n", s.name, s.ver.Version)
	return nil
}

// targetPath returns the node_modules directory for name, refusing any name
// that is not a valid package name or that would resolve outside
// node_modules. It is the last line of defence before any destructive
// filesystem operation.
func (in *Installer) targetPath(name string) (string, error) {
	if err := resolver.ValidateName(name); err != nil {
		return "", err
	}
	nodeModules := in.nodeModulesDir()
	target := filepath.Join(nodeModules, name)
	if !strings.HasPrefix(target, filepath.Clean(nodeModules)+string(os.PathSeparator)) {
		return "", fmt.Errorf("refusing to install %q outside node_modules", name)
	}
	return target, nil
}

// warnIfIncompatible prints a warning if version does not satisfy rng and
// rng is checkable.
func (in *Installer) warnIfIncompatible(name, version, rng string) {
	if !compatible(version, rng) {
		fmt.Fprintf(in.errOut(), "warn: %s@%s already installed, wanted %s (skipping)\n", name, version, rng)
	}
}

// compatible reports whether version satisfies rng. A dist-tag range (e.g.
// "latest") can't be checked against an already-installed version without
// a registry lookup, so such ranges are treated as compatible rather than
// producing a spurious warning.
func compatible(version, rng string) bool {
	if !resolver.IsRange(rng) {
		return true
	}
	return resolver.Satisfies(version, rng)
}

// preinstalled returns the manifest of name as already present in
// node_modules. It returns (nil, nil) only when the package is genuinely
// absent; an unreadable, unparsable or version-less package.json is
// reported as an error so the caller can warn and reinstall instead of
// silently trusting a broken directory.
func (in *Installer) preinstalled(name string) (*manifest.Manifest, error) {
	target, err := in.targetPath(name)
	if err != nil {
		return nil, err
	}
	m, err := manifest.Load(filepath.Join(target, "package.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if m.Version == "" {
		return nil, errors.New("no version field")
	}
	return m, nil
}

// isUnsupportedSpec reports whether rng is a dependency spec bale v1 does
// not support: git/http(s) URLs, file:/link:/npm: protocol specs, or a
// GitHub "user/repo" shorthand.
func isUnsupportedSpec(rng string) bool {
	for _, prefix := range []string{"git", "http", "file:", "link:", "npm:", "github:"} {
		if strings.HasPrefix(rng, prefix) {
			return true
		}
	}
	if strings.Contains(rng, "/") && !strings.HasPrefix(rng, "@") {
		return true
	}
	return false
}
