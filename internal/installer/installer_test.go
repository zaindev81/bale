package installer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"bale/internal/manifest"
	"bale/internal/registry"
)

// fakePackage describes one published version served by the fake registry.
type fakePackage struct {
	name    string
	version string
	deps    map[string]string

	// tarballVersion overrides the version written into the tarball's
	// package.json, so tests can serve an archive that disagrees with the
	// registry metadata. Empty means "same as version".
	tarballVersion string
}

// tarballKey is the fake registry's identifier for one package version. The
// slash of a scoped name is replaced so the key is a single URL path segment.
func tarballKey(name, version string) string {
	return strings.ReplaceAll(name, "/", "_") + "-" + version
}

// buildTarball builds an in-memory .tgz containing package/package.json for
// the given fake package.
func buildTarball(t *testing.T, p fakePackage) []byte {
	t.Helper()
	version := p.tarballVersion
	if version == "" {
		version = p.version
	}
	m := manifest.Manifest{Name: p.name, Version: version, Dependencies: p.deps}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal package.json: %v", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "package/package.json",
		Mode: 0644,
		Size: int64(len(body)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

// fakeRegistry is a fake npm registry backed by httptest.Server, and also
// records which tarballs it has served so tests can assert a package was
// (or was not) re-downloaded.
type fakeRegistry struct {
	srv *httptest.Server

	mu       sync.Mutex
	tarballs []string // requested tarball keys, e.g. "b-1.1.0"
}

// tarballFetchedFor reports whether any version of name had its tarball
// requested since the log was last reset.
func (fr *fakeRegistry) tarballFetchedFor(name string) bool {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	prefix := strings.ReplaceAll(name, "/", "_") + "-"
	for _, key := range fr.tarballs {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// resetTarballLog clears the record of requested tarballs.
func (fr *fakeRegistry) resetTarballLog() {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.tarballs = nil
}

// newFakeRegistry serves packuments and tarballs for the given packages,
// grouped by package name, with correctly computed sha512 integrity.
// extraTags adds additional dist-tags per package name (beyond the
// automatic "latest"); it may be nil.
//
// Packuments are served by a single catch-all handler that resolves the
// package name from the request path, because the client encodes a scoped
// name as "@scope%2fpkg" rather than as two path segments.
func newFakeRegistry(t *testing.T, pkgs map[string][]fakePackage, extraTags map[string]map[string]string) *fakeRegistry {
	t.Helper()
	fr := &fakeRegistry{}

	// Everything a handler reads is built before the server starts (or,
	// for packuments, before this function returns), so no handler ever
	// races with test setup.
	tarballBodies := make(map[string][]byte)
	integrities := make(map[string]string)
	for _, versions := range pkgs {
		for _, v := range versions {
			tgz := buildTarball(t, v)
			key := tarballKey(v.name, v.version)
			tarballBodies[key] = tgz
			sum := sha512.Sum512(tgz)
			integrities[key] = "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
		}
	}
	packuments := make(map[string][]byte)

	mux := http.NewServeMux()

	mux.HandleFunc("/tarballs/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/tarballs/"), ".tgz")
		fr.mu.Lock()
		fr.tarballs = append(fr.tarballs, key)
		fr.mu.Unlock()

		body, ok := tarballBodies[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, name := range requestedNames(r) {
			if doc, ok := packuments[name]; ok {
				w.Header().Set("Content-Type", "application/json")
				w.Write(doc)
				return
			}
		}
		http.NotFound(w, r)
	})

	srv := httptest.NewServer(mux)
	fr.srv = srv

	for name, versions := range pkgs {
		p := registry.Packument{
			Name:     name,
			DistTags: map[string]string{},
			Versions: map[string]registry.Version{},
		}
		var highest string
		for _, v := range versions {
			key := tarballKey(v.name, v.version)
			p.Versions[v.version] = registry.Version{
				Name:         name,
				Version:      v.version,
				Dependencies: v.deps,
				Dist: registry.Dist{
					Tarball:   srv.URL + "/tarballs/" + key + ".tgz",
					Integrity: integrities[key],
				},
			}
			highest = v.version
		}
		p.DistTags["latest"] = highest
		for tag, ver := range extraTags[name] {
			p.DistTags[tag] = ver
		}
		doc, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshal packument for %s: %v", name, err)
		}
		packuments[name] = doc
	}

	return fr
}

// requestedNames returns the package names a packument request could be
// asking for, decoded from both the raw and the already-unescaped path.
func requestedNames(r *http.Request) []string {
	candidates := []string{strings.TrimPrefix(r.URL.Path, "/")}
	if r.URL.RawPath != "" {
		raw := strings.TrimPrefix(r.URL.RawPath, "/")
		candidates = append(candidates, raw)
		if unescaped, err := url.PathUnescape(raw); err == nil {
			candidates = append(candidates, unescaped)
		}
	}
	return candidates
}

// newTestInstaller sets up an Installer for dir against a fake registry
// serving the given packages and extra dist-tags, writing progress to out
// and warnings to errOut.
func newTestInstaller(t *testing.T, dir string, pkgs map[string][]fakePackage, extraTags map[string]map[string]string, out, errOut *bytes.Buffer) (*Installer, *fakeRegistry) {
	t.Helper()
	fr := newFakeRegistry(t, pkgs, extraTags)
	t.Cleanup(fr.srv.Close)
	in := New(dir, registry.NewClient(fr.srv.URL))
	in.Out = out
	in.ErrOut = errOut
	return in, fr
}

// fakePkgs returns a: 1.0.0 (deps on b@^1.0.0), b: 1.0.0/1.1.0, and
// c: 1.0.0 (deps on b@^2.0.0, which nothing satisfies).
func fakePkgs() map[string][]fakePackage {
	return map[string][]fakePackage{
		"a": {
			{name: "a", version: "1.0.0", deps: map[string]string{"b": "^1.0.0"}},
		},
		"b": {
			{name: "b", version: "1.0.0"},
			{name: "b", version: "1.1.0"},
		},
		"c": {
			{name: "c", version: "1.0.0", deps: map[string]string{"b": "^2.0.0"}},
		},
	}
}

func TestAddInstallsTransitiveDeps(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)

	if err := in.Add(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	aVer := readInstalledVersion(t, dir, "a")
	if aVer != "1.0.0" {
		t.Errorf("a version = %q, want 1.0.0", aVer)
	}
	bVer := readInstalledVersion(t, dir, "b")
	if bVer != "1.1.0" {
		t.Errorf("b version = %q, want 1.1.0", bVer)
	}

	m := readManifest(t, dir)
	if m.Dependencies["a"] != "^1.0.0" {
		t.Errorf(`Dependencies["a"] = %q, want "^1.0.0"`, m.Dependencies["a"])
	}
}

// TestAddCreatesManifest verifies that adding a package to a directory with
// no package.json creates one, rather than requiring it to exist first.
func TestAddCreatesManifest(t *testing.T) {
	dir := t.TempDir()

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)

	if err := in.Add(context.Background(), []string{"b@1.0.0"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	m := readManifest(t, dir)
	if m.Dependencies["b"] != "1.0.0" {
		t.Errorf(`Dependencies["b"] = %q, want "1.0.0"`, m.Dependencies["b"])
	}
}

// TestAddSecondCallReinstallsExplicitTopLevel verifies that a top-level
// `bale install <pkg>` always re-resolves and reinstalls, even when a
// different (incompatible) version is already present from a transitive
// install - like `npm install foo` does.
func TestAddSecondCallReinstallsExplicitTopLevel(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)

	if err := in.Add(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Add(a): %v", err)
	}

	out.Reset()
	errOut.Reset()
	if err := in.Add(context.Background(), []string{"b@1.0.0"}); err != nil {
		t.Fatalf("Add(b@1.0.0): %v", err)
	}

	got := out.String()
	if strings.Contains(errOut.String(), "warn:") {
		t.Errorf("expected no warn output for an explicit top-level reinstall, got %q", errOut.String())
	}
	if !strings.Contains(got, "+ b@1.0.0") {
		t.Errorf("expected b to be reinstalled at 1.0.0, got %q", got)
	}

	bVer := readInstalledVersion(t, dir, "b")
	if bVer != "1.0.0" {
		t.Errorf("b version = %q, want 1.0.0 (explicit top-level request wins)", bVer)
	}

	m := readManifest(t, dir)
	if m.Dependencies["b"] != "1.0.0" {
		t.Errorf(`Dependencies["b"] = %q, want "1.0.0"`, m.Dependencies["b"])
	}
}

// TestTransitiveDepReusesPreinstalled verifies that a transitive dependency
// already satisfied by what's in node_modules is reused without ever
// contacting the registry for its tarball.
func TestTransitiveDepReusesPreinstalled(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	var out, errOut bytes.Buffer
	in, fr := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)

	if err := in.Add(context.Background(), []string{"b@1.0.0"}); err != nil {
		t.Fatalf("Add(b@1.0.0): %v", err)
	}
	if !fr.tarballFetchedFor("b") {
		t.Fatal("expected b's tarball to be fetched on its first install")
	}

	fr.resetTarballLog()
	out.Reset()

	if err := in.Add(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Add(a): %v", err)
	}

	bVer := readInstalledVersion(t, dir, "b")
	if bVer != "1.0.0" {
		t.Errorf("b version = %q, want 1.0.0 (transitive reuse)", bVer)
	}
	if fr.tarballFetchedFor("b") {
		t.Errorf("expected b's tarball NOT to be fetched when reused transitively")
	}
}

// TestTransitiveConflictWarns verifies that a transitive requirement that
// conflicts with an already-installed version only warns - it neither
// errors nor fetches a version that doesn't exist - and that the warning
// goes to ErrOut rather than to the progress stream.
func TestTransitiveConflictWarns(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	var out, errOut bytes.Buffer
	in, fr := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)

	if err := in.Add(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Add(a): %v", err)
	}
	if readInstalledVersion(t, dir, "b") != "1.1.0" {
		t.Fatalf("precondition failed: b not installed at 1.1.0")
	}

	fr.resetTarballLog()
	out.Reset()
	errOut.Reset()

	if err := in.Add(context.Background(), []string{"c"}); err != nil {
		t.Fatalf("Add(c): %v", err)
	}

	got := errOut.String()
	if !strings.Contains(got, "warn:") || !strings.Contains(got, "b@1.1.0") || !strings.Contains(got, "^2.0.0") {
		t.Errorf("expected a warn on ErrOut mentioning b@1.1.0 and ^2.0.0, got %q", got)
	}
	if strings.Contains(out.String(), "warn:") {
		t.Errorf("warnings must not go to Out, got %q", out.String())
	}

	bVer := readInstalledVersion(t, dir, "b")
	if bVer != "1.1.0" {
		t.Errorf("b version = %q, want unchanged 1.1.0", bVer)
	}
	if fr.tarballFetchedFor("b") {
		t.Errorf("expected b's tarball NOT to be fetched for an unsatisfiable transitive range")
	}
}

// TestUnreadablePreinstalledWarnsAndReinstalls verifies that a corrupt
// package.json in node_modules is reported on ErrOut and the package is
// reinstalled instead of being silently trusted or silently skipped.
func TestUnreadablePreinstalledWarnsAndReinstalls(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	var out, errOut bytes.Buffer
	in, fr := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)

	if err := in.Add(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Add(a): %v", err)
	}

	corrupt := filepath.Join(dir, "node_modules", "b", "package.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0644); err != nil {
		t.Fatalf("corrupt b: %v", err)
	}

	fr.resetTarballLog()
	out.Reset()
	errOut.Reset()

	if err := in.Add(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Add(a) second: %v", err)
	}

	if !strings.Contains(errOut.String(), "warn:") || !strings.Contains(errOut.String(), "unreadable") {
		t.Errorf("expected an unreadable warning on ErrOut, got %q", errOut.String())
	}
	if strings.Contains(out.String(), "warn:") {
		t.Errorf("warnings must not go to Out, got %q", out.String())
	}
	if !fr.tarballFetchedFor("b") {
		t.Error("expected b to be reinstalled after its package.json became unreadable")
	}
	if readInstalledVersion(t, dir, "b") != "1.1.0" {
		t.Error("expected b to be restored to 1.1.0")
	}
}

// TestAddDistTagSavesCaret verifies that resolving a custom dist-tag (not
// just "latest") saves the dependency range as "^<resolved version>".
func TestAddDistTagSavesCaret(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	var out, errOut bytes.Buffer
	tags := map[string]map[string]string{"b": {"beta": "1.1.0"}}
	in, _ := newTestInstaller(t, dir, fakePkgs(), tags, &out, &errOut)

	if err := in.Add(context.Background(), []string{"b@beta"}); err != nil {
		t.Fatalf("Add(b@beta): %v", err)
	}

	m := readManifest(t, dir)
	if m.Dependencies["b"] != "^1.1.0" {
		t.Errorf(`Dependencies["b"] = %q, want "^1.1.0"`, m.Dependencies["b"])
	}
}

// TestAddRepeatIsIdempotent verifies that adding the same top-level package
// twice reinstalls it cleanly both times, without warning.
func TestAddRepeatIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)

	if err := in.Add(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Add(a) first: %v", err)
	}

	out.Reset()
	errOut.Reset()
	if err := in.Add(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("Add(a) second: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "+ a@1.0.0") {
		t.Errorf("expected a reinstall line for a, got %q", got)
	}
	if strings.Contains(errOut.String(), "warn:") {
		t.Errorf("expected no warn output, got %q", errOut.String())
	}

	m := readManifest(t, dir)
	if m.Dependencies["a"] != "^1.0.0" {
		t.Errorf(`Dependencies["a"] = %q, want "^1.0.0"`, m.Dependencies["a"])
	}
}

// TestAddKeepsDevDependency verifies that re-adding a package already listed
// in devDependencies updates it there instead of duplicating it into
// dependencies.
func TestAddKeepsDevDependency(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{
		Name:            "proj",
		Version:         "1.0.0",
		DevDependencies: map[string]string{"b": "1.0.0"},
	})

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)

	if err := in.Add(context.Background(), []string{"b@1.1.0"}); err != nil {
		t.Fatalf("Add(b@1.1.0): %v", err)
	}

	m := readManifest(t, dir)
	if m.DevDependencies["b"] != "1.1.0" {
		t.Errorf(`DevDependencies["b"] = %q, want "1.1.0"`, m.DevDependencies["b"])
	}
	if _, ok := m.Dependencies["b"]; ok {
		t.Errorf("b must not be duplicated into dependencies: %v", m.Dependencies)
	}
}

// TestAddScopedPackage verifies a scoped package installs into
// node_modules/@scope/pkg and is recorded with a caret range.
func TestAddScopedPackage(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	pkgs := map[string][]fakePackage{
		"@scope/pkg": {{name: "@scope/pkg", version: "2.3.4"}},
	}

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, pkgs, nil, &out, &errOut)

	if err := in.Add(context.Background(), []string{"@scope/pkg"}); err != nil {
		t.Fatalf("Add(@scope/pkg): %v", err)
	}

	installed := filepath.Join(dir, "node_modules", "@scope", "pkg", "package.json")
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("expected %s to exist: %v", installed, err)
	}
	if got := readInstalledVersion(t, dir, "@scope/pkg"); got != "2.3.4" {
		t.Errorf("@scope/pkg version = %q, want 2.3.4", got)
	}

	m := readManifest(t, dir)
	if m.Dependencies["@scope/pkg"] != "^2.3.4" {
		t.Errorf(`Dependencies["@scope/pkg"] = %q, want "^2.3.4"`, m.Dependencies["@scope/pkg"])
	}
}

// TestMaliciousDependencyNameRejected verifies that a dependency name from
// registry metadata that would escape node_modules is refused before
// anything is written, and that the file it targeted is untouched.
func TestMaliciousDependencyNameRejected(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "proj")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	// node_modules/../../victim resolves to base/victim.
	victim := filepath.Join(base, "victim")
	if err := os.WriteFile(victim, []byte("do not touch"), 0644); err != nil {
		t.Fatalf("write victim: %v", err)
	}

	pkgs := map[string][]fakePackage{
		"evil": {{name: "evil", version: "1.0.0", deps: map[string]string{"../../victim": "1.0.0"}}},
	}

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, pkgs, nil, &out, &errOut)

	err := in.Add(context.Background(), []string{"evil"})
	if err == nil {
		t.Fatal("expected an error for a path-traversal dependency name")
	}
	if !strings.Contains(err.Error(), "invalid dependency name") {
		t.Errorf("error = %q, want mention of an invalid dependency name", err)
	}
	if !strings.Contains(err.Error(), "evil@1.0.0") {
		t.Errorf("error = %q, want the declaring package named", err)
	}

	data, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatalf("victim file was disturbed: %v", readErr)
	}
	if string(data) != "do not touch" {
		t.Errorf("victim contents = %q, want unchanged", data)
	}
	if entries, err := os.ReadDir(base); err == nil {
		for _, e := range entries {
			if e.Name() != "proj" && e.Name() != "victim" {
				t.Errorf("unexpected entry created outside the project: %s", e.Name())
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "evil")); err == nil {
		t.Error("nothing should have been installed")
	}
}

// TestDotDependencyNameRejected verifies that "." is refused as a dependency
// name (it would otherwise resolve to node_modules itself).
func TestDotDependencyNameRejected(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	pkgs := map[string][]fakePackage{
		"dotty": {{name: "dotty", version: "1.0.0", deps: map[string]string{".": "1.0.0"}}},
	}

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, pkgs, nil, &out, &errOut)

	err := in.Add(context.Background(), []string{"dotty"})
	if err == nil {
		t.Fatal(`expected an error for a dependency named "."`)
	}
	if !strings.Contains(err.Error(), "invalid dependency name") {
		t.Errorf("error = %q, want mention of an invalid dependency name", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "dotty")); err == nil {
		t.Error("nothing should have been installed")
	}
}

// TestAddFailsBeforeWritingAnything verifies the two-phase install: if any
// package in the request cannot be resolved, nothing at all is written to
// node_modules.
func TestAddFailsBeforeWritingAnything(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	pkgs := map[string][]fakePackage{
		"good": {{name: "good", version: "1.0.0"}},
	}

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, pkgs, nil, &out, &errOut)

	err := in.Add(context.Background(), []string{"good", "nosuch"})
	if err == nil {
		t.Fatal("expected an error for a package that does not exist")
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "good")); err == nil {
		t.Error("good must not be installed when a sibling request fails to resolve")
	}
	m := readManifest(t, dir)
	if _, ok := m.Dependencies["good"]; ok {
		t.Error("package.json must not record good when the install failed")
	}
}

// TestTarballVersionMismatch verifies that a tarball whose package.json
// disagrees with the resolved version is rejected, leaves no package
// behind, and cleans up its staging directory.
func TestTarballVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	pkgs := map[string][]fakePackage{
		"liar": {{name: "liar", version: "1.0.0", tarballVersion: "9.9.9"}},
	}

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, pkgs, nil, &out, &errOut)

	err := in.Add(context.Background(), []string{"liar"})
	if err == nil {
		t.Fatal("expected an error for a version mismatch between tarball and metadata")
	}
	if !strings.Contains(err.Error(), "tarball reports version") {
		t.Errorf("error = %q, want mention of the tarball version", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "liar")); err == nil {
		t.Error("a package that failed verification must not be installed")
	}

	entries, err := os.ReadDir(filepath.Join(dir, "node_modules"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read node_modules: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bale-tmp-") {
			t.Errorf("staging directory left behind: %s", e.Name())
		}
	}
}

func TestInstallAllFromManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{
		Name:         "proj",
		Version:      "1.0.0",
		Dependencies: map[string]string{"a": "^1.0.0"},
	})

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)

	if err := in.InstallAll(context.Background()); err != nil {
		t.Fatalf("InstallAll: %v", err)
	}

	if readInstalledVersion(t, dir, "a") != "1.0.0" {
		t.Errorf("a not installed at expected version")
	}
	if readInstalledVersion(t, dir, "b") != "1.1.0" {
		t.Errorf("b not installed at expected version")
	}
}

// TestInstallAllCompletesIncompleteTree verifies that a transitive package
// deleted from node_modules is reinstalled by a plain `bale install`, by
// following the dependencies of packages that are already present.
func TestInstallAllCompletesIncompleteTree(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{
		Name:         "proj",
		Version:      "1.0.0",
		Dependencies: map[string]string{"a": "^1.0.0"},
	})

	pkgs := map[string][]fakePackage{
		"a": {{name: "a", version: "1.0.0", deps: map[string]string{"b": "^1.0.0"}}},
		"b": {{name: "b", version: "1.0.0", deps: map[string]string{"c": "^1.0.0"}}},
		"c": {{name: "c", version: "1.0.0"}},
	}

	var out, errOut bytes.Buffer
	in, fr := newTestInstaller(t, dir, pkgs, nil, &out, &errOut)

	if err := in.InstallAll(context.Background()); err != nil {
		t.Fatalf("InstallAll: %v", err)
	}
	if readInstalledVersion(t, dir, "c") != "1.0.0" {
		t.Fatal("precondition failed: c not installed")
	}

	if err := os.RemoveAll(filepath.Join(dir, "node_modules", "c")); err != nil {
		t.Fatalf("remove c: %v", err)
	}
	fr.resetTarballLog()
	out.Reset()

	if err := in.InstallAll(context.Background()); err != nil {
		t.Fatalf("InstallAll (repair): %v", err)
	}

	if readInstalledVersion(t, dir, "c") != "1.0.0" {
		t.Error("c should have been reinstalled")
	}
	if !fr.tarballFetchedFor("c") {
		t.Error("expected c's tarball to be fetched during the repair")
	}
	if fr.tarballFetchedFor("a") || fr.tarballFetchedFor("b") {
		t.Error("packages already present should not be re-downloaded")
	}
}

func TestInstallAllRepairsDirectVersionDrift(t *testing.T) {
	for _, tt := range []struct {
		name, pkg, installed, wanted, resolved string
		dev                                    bool
	}{
		{"upgrade", "a", "1.0.0", "^2.0.0", "2.0.0", false},
		{"downgrade", "a", "2.0.0", "1.0.0", "1.0.0", false},
		{"dev dependency", "a", "1.0.0", "^2.0.0", "2.0.0", true},
		{"scoped dependency", "@scope/a", "1.0.0", "^2.0.0", "2.0.0", false},
		{"invalid installed version", "a", "broken", "^2.0.0", "2.0.0", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			m := &manifest.Manifest{Name: "proj", Dependencies: map[string]string{tt.pkg: tt.wanted}}
			if tt.dev {
				m.DevDependencies, m.Dependencies = m.Dependencies, nil
			}
			writeManifest(t, dir, m)
			installedDir := filepath.Join(dir, "node_modules", tt.pkg)
			if err := os.MkdirAll(installedDir, 0755); err != nil {
				t.Fatal(err)
			}
			// The obsolete dependency must not be resolved from the old manifest.
			writeManifest(t, installedDir, &manifest.Manifest{
				Name: tt.pkg, Version: tt.installed, Dependencies: map[string]string{"obsolete": "*"},
			})
			pkgs := map[string][]fakePackage{
				tt.pkg: {{name: tt.pkg, version: tt.resolved, deps: map[string]string{"b": "^1.0.0"}}},
				"b":    {{name: "b", version: "1.0.0"}},
			}
			before, err := os.ReadFile(filepath.Join(dir, "package.json"))
			if err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			in, _ := newTestInstaller(t, dir, pkgs, nil, &out, &errOut)
			if err := in.InstallAll(context.Background()); err != nil {
				t.Fatalf("InstallAll: %v", err)
			}
			if got := readInstalledVersion(t, dir, tt.pkg); got != tt.resolved {
				t.Errorf("installed version = %q, want %q", got, tt.resolved)
			}
			if got := readInstalledVersion(t, dir, "b"); got != "1.0.0" {
				t.Errorf("new transitive dependency version = %q, want 1.0.0", got)
			}
			after, err := os.ReadFile(filepath.Join(dir, "package.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("InstallAll changed the project manifest")
			}
			if errOut.Len() != 0 {
				t.Errorf("unexpected warnings: %s", &errOut)
			}
		})
	}
}

func TestInstallAllReusesCompatibleTreeOffline(t *testing.T) {
	for _, rng := range []string{"^1.0.0", "1.0.0", "*", "", "latest"} {
		t.Run(rng, func(t *testing.T) {
			dir := t.TempDir()
			var out, errOut bytes.Buffer
			in, fr := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)
			if err := in.Add(context.Background(), []string{"a@1.0.0"}); err != nil {
				t.Fatal(err)
			}
			writeManifest(t, dir, &manifest.Manifest{Dependencies: map[string]string{"a": rng}})
			fr.srv.Close()
			out.Reset()
			if err := in.InstallAll(context.Background()); err != nil {
				t.Fatalf("InstallAll with registry offline: %v", err)
			}
			if out.Len() != 0 || errOut.Len() != 0 {
				t.Errorf("expected silent reuse, got output %q and warnings %q", &out, &errOut)
			}
		})
	}
}

func TestInstallAllFailedRepairPreservesExistingPackages(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	pkgs := fakePkgs()
	pkgs["a"] = append(pkgs["a"], fakePackage{
		name: "a", version: "2.0.0", deps: map[string]string{"missing": "^1.0.0"},
	})
	in, fr := newTestInstaller(t, dir, pkgs, nil, &out, &errOut)
	if err := in.Add(context.Background(), []string{"a@1.0.0"}); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, dir, &manifest.Manifest{Dependencies: map[string]string{"a": "^2.0.0"}})
	fr.resetTarballLog()
	out.Reset()
	err := in.InstallAll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("InstallAll error = %v, want missing dependency", err)
	}
	if got := readInstalledVersion(t, dir, "a"); got != "1.0.0" {
		t.Errorf("failed repair replaced a with %q", got)
	}
	if got := readInstalledVersion(t, dir, "b"); got != "1.1.0" {
		t.Errorf("failed repair changed b to %q", got)
	}
	if fr.tarballFetchedFor("a") || out.Len() != 0 {
		t.Error("failed resolution should not start applying the repair")
	}
}

func TestInstallUnsupportedSpec(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{
		Name:         "proj",
		Version:      "1.0.0",
		Dependencies: map[string]string{"weird": "git+https://example.com/weird.git"},
	})

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)

	err := in.InstallAll(context.Background())
	if err == nil {
		t.Fatal("expected error for unsupported dependency spec")
	}
	if !strings.Contains(err.Error(), "unsupported dependency spec") {
		t.Errorf("error = %q, want mention of unsupported dependency spec", err)
	}
}

// TestInstallMalformedRangeErrorsEvenWhenInstalled verifies that a bad
// range in package.json is an error regardless of what happens to be
// present in node_modules already.
func TestInstallMalformedRangeErrorsEvenWhenInstalled(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, &manifest.Manifest{Name: "proj", Version: "1.0.0"})

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)

	if err := in.Add(context.Background(), []string{"b@1.0.0"}); err != nil {
		t.Fatalf("Add(b@1.0.0): %v", err)
	}
	if readInstalledVersion(t, dir, "b") != "1.0.0" {
		t.Fatal("precondition failed: b not installed")
	}

	writeManifest(t, dir, &manifest.Manifest{
		Name:         "proj",
		Version:      "1.0.0",
		Dependencies: map[string]string{"b": "^^1.0.0"},
	})

	err := in.InstallAll(context.Background())
	if err == nil {
		t.Fatal("expected an error for a malformed version range")
	}
	if !strings.Contains(err.Error(), "invalid version range") {
		t.Errorf("error = %q, want mention of an invalid version range", err)
	}
}

// TestInstallAllWithoutManifest verifies the error when there is no
// package.json to install from.
func TestInstallAllWithoutManifest(t *testing.T) {
	dir := t.TempDir()

	var out, errOut bytes.Buffer
	in, _ := newTestInstaller(t, dir, fakePkgs(), nil, &out, &errOut)

	err := in.InstallAll(context.Background())
	if err == nil {
		t.Fatal("expected an error when package.json is missing")
	}
	if !strings.Contains(err.Error(), "no package.json found in") {
		t.Errorf("error = %q, want mention of a missing package.json", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		t.Error("install must not create a package.json")
	}
}

func writeManifest(t *testing.T, dir string, m *manifest.Manifest) {
	t.Helper()
	if err := m.Save(filepath.Join(dir, "package.json")); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func readManifest(t *testing.T, dir string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Load(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	return m
}

func readInstalledVersion(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, "node_modules", name, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m.Version
}
