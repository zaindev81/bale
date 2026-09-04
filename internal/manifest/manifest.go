// Package manifest reads and writes package.json files.
package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Manifest represents the subset of package.json fields bale understands:
// name, version, and the two dependency maps. Every other field —
// description, main, scripts, license, and the many non-standard shapes
// real npm packages publish — is kept verbatim in Extra so that loading and
// saving a package.json never drops or reinterprets it.
type Manifest struct {
	Name            string            `json:"name,omitempty"`
	Version         string            `json:"version,omitempty"`
	Dependencies    map[string]string `json:"dependencies,omitempty"`
	DevDependencies map[string]string `json:"devDependencies,omitempty"`

	// Extra holds any top-level fields not listed above (for example
	// "type", "exports", "main", "scripts" or "license"), keyed by their
	// JSON name.
	Extra map[string]json.RawMessage `json:"-"`
}

// knownFields lists the JSON keys covered by the typed fields of Manifest.
var knownFields = []string{
	"name", "version", "dependencies", "devDependencies",
}

// isKnownField reports whether key is one of knownFields.
func isKnownField(key string) bool {
	for _, known := range knownFields {
		if key == known {
			return true
		}
	}
	return false
}

// plain is Manifest without its methods, so the typed fields can be
// (un)marshaled with the default encoding without recursing.
type plain Manifest

// UnmarshalJSON decodes the typed fields and stashes everything else in
// Extra. name and version are decoded strictly as strings: the registry
// guarantees their shape, so a mismatch there is a genuine error worth
// surfacing. dependencies and devDependencies are decoded leniently via
// decodeDeps; see its comment for why.
func (m *Manifest) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var p plain
	if nameRaw, ok := raw["name"]; ok {
		if err := json.Unmarshal(nameRaw, &p.Name); err != nil {
			return fmt.Errorf("field %q: %w", "name", err)
		}
	}
	if versionRaw, ok := raw["version"]; ok {
		if err := json.Unmarshal(versionRaw, &p.Version); err != nil {
			return fmt.Errorf("field %q: %w", "version", err)
		}
	}
	p.Dependencies = decodeDeps(raw["dependencies"])
	p.DevDependencies = decodeDeps(raw["devDependencies"])

	for _, key := range knownFields {
		delete(raw, key)
	}
	if len(raw) == 0 {
		raw = nil
	}
	p.Extra = raw
	*m = Manifest(p)
	return nil
}

// decodeDeps decodes a "dependencies" or "devDependencies" field the way
// npm's own package.json reader does: leniently. These maps come from
// package.json files inside third-party tarballs downloaded from the
// registry, not from bale itself, and years of publishing mistakes mean the
// field doesn't always look the way npm expects. Some old packages publish
// "dependencies": [] instead of {}; some have non-string values (numbers,
// null, nested objects) for individual entries. A package manager has to be
// able to install these packages anyway, so anything that isn't a JSON
// object degrades to "no dependencies" and an individual entry with a
// non-string value is simply skipped, rather than failing the whole load.
func decodeDeps(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		// Not a JSON object at all (e.g. "dependencies": []): treat as empty.
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	deps := make(map[string]string, len(entries))
	for name, v := range entries {
		// Unmarshaling JSON null into a string silently succeeds and
		// leaves it empty rather than erroring, so a non-string value has
		// to be recognized up front: only decode entries that are
		// actually JSON strings, and skip everything else (null,
		// numbers, booleans, objects, arrays).
		trimmed := bytes.TrimSpace(v)
		if len(trimmed) == 0 || trimmed[0] != '"' {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			// Malformed string literal: skip this entry, keep the rest.
			continue
		}
		deps[name] = s
	}
	if len(deps) == 0 {
		return nil
	}
	return deps
}

// MarshalJSON encodes the typed fields first, in declaration order, followed
// by the Extra fields sorted by key. HTML characters are not escaped.
func (m Manifest) MarshalJSON() ([]byte, error) {
	known, err := marshalNoEscape(plain(m))
	if err != nil {
		return nil, err
	}
	if len(m.Extra) == 0 {
		return known, nil
	}

	keys := make([]string, 0, len(m.Extra))
	for key := range m.Extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	buf.Write(known[:len(known)-1]) // drop the closing brace
	for _, key := range keys {
		if buf.Len() > 1 {
			buf.WriteByte(',')
		}
		name, err := marshalNoEscape(key)
		if err != nil {
			return nil, err
		}
		buf.Write(name)
		buf.WriteByte(':')
		buf.Write(m.Extra[key])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalNoEscape is json.Marshal without HTML escaping, so that strings
// such as `a && b` survive a round trip unchanged.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Load reads and parses the package.json file at path.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load manifest %s: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("load manifest %s: %w", path, err)
	}
	return &m, nil
}

// defaultManifestMode is the permission a newly created package.json gets.
const defaultManifestMode os.FileMode = 0644

// Save writes m to path as two-space indented JSON with a trailing newline.
//
// The write is atomic: the content goes to a temporary file in the same
// directory, is flushed to disk, and is then renamed over path, so a crash
// or a full disk can never leave a half-written package.json behind. An
// existing file keeps its permissions; a new one is created 0644.
func (m *Manifest) Save(path string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return fmt.Errorf("save manifest %s: %w", path, err)
	}

	mode := defaultManifestMode
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	if err := writeFileAtomic(path, buf.Bytes(), mode); err != nil {
		return fmt.Errorf("save manifest %s: %w", path, err)
	}
	return nil
}

// writeFileAtomic writes data to path via a temporary file in the same
// directory, removing that temporary file if anything goes wrong.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	// From here on every failure must clean up after itself.
	fail := func(err error) error {
		_ = tmp.Close() // may already be closed; the original error wins
		_ = os.Remove(tmpName)
		return err
	}

	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	// CreateTemp makes the file 0600; restore the mode the manifest should
	// have before it becomes visible under its real name.
	if err := os.Chmod(tmpName, mode); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fail(err)
	}
	return nil
}

// AddDependency records name at range rng in the manifest's dependencies,
// initializing the map if necessary.
func (m *Manifest) AddDependency(name, rng string) {
	if m.Dependencies == nil {
		m.Dependencies = make(map[string]string)
	}
	m.Dependencies[name] = rng
}

// SetExtra JSON-encodes v, without HTML escaping, into Extra[key],
// initializing the map if necessary. It returns an error if key names one
// of the typed fields (name, version, dependencies, devDependencies), since
// those must be set through the corresponding struct field instead.
func (m *Manifest) SetExtra(key string, v any) error {
	if isKnownField(key) {
		return fmt.Errorf("manifest: %q is a known field; set it directly instead of via SetExtra", key)
	}
	data, err := marshalNoEscape(v)
	if err != nil {
		return fmt.Errorf("manifest: encode %q: %w", key, err)
	}
	if m.Extra == nil {
		m.Extra = make(map[string]json.RawMessage)
	}
	m.Extra[key] = data
	return nil
}
