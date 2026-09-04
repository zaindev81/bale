package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	content := `{
  "name": "foo",
  "version": "1.0.0",
  "dependencies": {
    "bar": "^1.0.0"
  }
}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Name != "foo" {
		t.Errorf("Name = %q, want foo", m.Name)
	}
	if m.Version != "1.0.0" {
		t.Errorf("Version = %q, want 1.0.0", m.Version)
	}
	if m.Dependencies["bar"] != "^1.0.0" {
		t.Errorf("Dependencies[bar] = %q, want ^1.0.0", m.Dependencies["bar"])
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "nope.json") {
		t.Errorf("error %q does not mention path", err)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	if err := os.WriteFile(path, []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	m := &Manifest{Name: "foo", Version: "1.0.0"}
	if err := m.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.HasSuffix(s, "\n") {
		t.Error("saved file does not end with newline")
	}
	if !strings.Contains(s, "  \"name\": \"foo\"") {
		t.Errorf("saved file not indented with 2 spaces:\n%s", s)
	}

	// Round trip.
	m2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if m2.Name != m.Name || m2.Version != m.Version {
		t.Errorf("round trip mismatch: got %+v, want %+v", m2, m)
	}
}

func TestSaveDependenciesSorted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	m := &Manifest{}
	m.AddDependency("zeta", "^1.0.0")
	m.AddDependency("alpha", "^1.0.0")
	m.AddDependency("mid", "^1.0.0")
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	ia := strings.Index(s, "\"alpha\"")
	im := strings.Index(s, "\"mid\"")
	iz := strings.Index(s, "\"zeta\"")
	if ia >= im || im >= iz {
		t.Errorf("dependencies not sorted alphabetically in output:\n%s", s)
	}
}

func TestAddDependencyInitializesMap(t *testing.T) {
	m := &Manifest{}
	if m.Dependencies != nil {
		t.Fatal("expected nil Dependencies initially")
	}
	m.AddDependency("foo", "^1.0.0")
	if m.Dependencies == nil {
		t.Fatal("expected Dependencies to be initialized")
	}
	if m.Dependencies["foo"] != "^1.0.0" {
		t.Errorf("Dependencies[foo] = %q, want ^1.0.0", m.Dependencies["foo"])
	}
}

func TestSaveDoesNotEscapeHTML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.json")
	m := &Manifest{}
	if err := m.SetExtra("scripts", map[string]string{"test": `echo "no tests" && exit 1`}); err != nil {
		t.Fatalf("SetExtra: %v", err)
	}
	if err := m.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `&& exit 1`) {
		t.Errorf("expected literal && in output, got:\n%s", data)
	}
	if strings.Contains(string(data), `\u0026`) {
		t.Errorf("output contains escaped &:\n%s", data)
	}
}

func TestRoundTripPreservesUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.json")
	content := `{
  "name": "foo",
  "version": "1.0.0",
  "type": "module",
  "exports": { ".": "./index.js" },
  "author": "someone <a@b.c>"
}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := string(m.Extra["type"]); got != `"module"` {
		t.Errorf(`Extra["type"] = %s, want "module"`, got)
	}
	if _, known := m.Extra["name"]; known {
		t.Error("known field leaked into Extra")
	}

	m.AddDependency("bar", "^1.0.0")
	if err := m.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		`"type": "module"`,
		`"exports": {`,
		`".": "./index.js"`,
		`"author": "someone <a@b.c>"`,
		`"bar": "^1.0.0"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("saved file missing %q:\n%s", want, s)
		}
	}
	if strings.Index(s, `"name"`) > strings.Index(s, `"type"`) {
		t.Errorf("known fields should precede extra fields:\n%s", s)
	}

	// Load again to make sure the merged output is valid JSON with all keys.
	m2, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if len(m2.Extra) != 3 {
		t.Errorf("Extra has %d keys after round trip, want 3", len(m2.Extra))
	}
	if m2.Dependencies["bar"] != "^1.0.0" {
		t.Errorf("dependency lost in round trip")
	}
}

func TestMarshalWithoutExtra(t *testing.T) {
	m := Manifest{Name: "foo"}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"name":"foo"}` {
		t.Errorf("Marshal = %s, want {\"name\":\"foo\"}", data)
	}
	empty := Manifest{Extra: map[string]json.RawMessage{"x": []byte(`1`)}}
	data, err = json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"x":1}` {
		t.Errorf("Marshal = %s, want {\"x\":1}", data)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	if err := os.WriteFile(path, []byte(`{"name":"old"}`), 0644); err != nil {
		t.Fatal(err)
	}

	m := &Manifest{Name: "new", Version: "2.0.0"}
	if err := m.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"name": "new"`) {
		t.Errorf("file was not replaced:\n%s", data)
	}

	// No temporary files may survive a successful save.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "package.json" {
			t.Errorf("leftover file in destination directory: %q", e.Name())
		}
	}
}

func TestSavePreservesExistingMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	// os.WriteFile respects umask on create, so force the mode explicitly.
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}

	if err := (&Manifest{Name: "foo"}).Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("mode = %v, want 0600", got)
	}
}

func TestSaveNewFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.json")
	if err := (&Manifest{Name: "foo"}).Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0644 {
		t.Errorf("mode = %v, want 0644", got)
	}
}

func TestSaveFailureLeavesNoTempFile(t *testing.T) {
	// A directory that does not exist makes CreateTemp fail; nothing should
	// be created and the error must name the manifest path.
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "package.json")
	if err := (&Manifest{}).Save(path); err == nil {
		t.Fatal("expected error saving into a missing directory")
	} else if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not mention path", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no files created, got %v", entries)
	}
}

// TestLoadToleratesNonStandardFieldTypes verifies that fields bale doesn't
// model can be any JSON type, not just the "usual" one. Real published npm
// packages do this: math-intrinsics ships "main": false, for example.
func TestLoadToleratesNonStandardFieldTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.json")
	content := `{"name":"x","version":"1.0.0","main":false,"scripts":[],"bin":{"x":"./x.js"}}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := string(m.Extra["main"]); got != "false" {
		t.Errorf(`Extra["main"] = %s, want false`, got)
	}
	if got := string(m.Extra["scripts"]); got != "[]" {
		t.Errorf(`Extra["scripts"] = %s, want []`, got)
	}
	if got := string(m.Extra["bin"]); got != `{"x":"./x.js"}` {
		t.Errorf(`Extra["bin"] = %s, want {"x":"./x.js"}`, got)
	}

	if err := m.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		`"main": false`,
		`"scripts": []`,
		`"bin": {`,
		`"x": "./x.js"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("saved file missing %q:\n%s", want, s)
		}
	}
}

// TestDependenciesArrayLoadsAsEmpty covers old packages that publish
// "dependencies": [] instead of {}.
func TestDependenciesArrayLoadsAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.json")
	content := `{"name":"x","version":"1.0.0","dependencies":[]}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Dependencies) != 0 {
		t.Errorf("Dependencies = %v, want empty", m.Dependencies)
	}
}

// TestDependenciesSkipsNonStringValues covers dependency maps with
// non-string entries, which should be dropped individually rather than
// failing the whole load.
func TestDependenciesSkipsNonStringValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "package.json")
	content := `{"name":"x","version":"1.0.0","dependencies":{"a":"^1.0.0","b":7,"c":null}}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Dependencies) != 1 || m.Dependencies["a"] != "^1.0.0" {
		t.Errorf("Dependencies = %v, want {a: ^1.0.0}", m.Dependencies)
	}
}

func TestSetExtraRejectsKnownField(t *testing.T) {
	m := &Manifest{}
	if err := m.SetExtra("name", "foo"); err == nil {
		t.Fatal("expected error setting a known field via SetExtra")
	}
	if err := m.SetExtra("dependencies", map[string]string{"a": "1.0.0"}); err == nil {
		t.Fatal("expected error setting a known field via SetExtra")
	}
}
