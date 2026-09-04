package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bale/internal/manifest"
)

// TestRun drives the CLI end to end, one temporary working directory per
// case, asserting the exit code and what lands on each stream.
func TestRun(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer notFound.Close()

	tests := []struct {
		name string
		args []string
		// setup runs in the (already current) temporary directory.
		setup          func(t *testing.T)
		want           int
		wantStdout     string
		wantStderr     string
		wantNoManifest bool
	}{
		{
			name:       "no arguments prints usage",
			args:       nil,
			want:       0,
			wantStdout: "bale - a minimal Node package manager",
		},
		{
			name:       "help subcommand",
			args:       []string{"help"},
			want:       0,
			wantStdout: "Usage:",
		},
		{
			name:       "long help flag",
			args:       []string{"--help"},
			want:       0,
			wantStdout: "Usage:",
		},
		{
			name:       "short help flag",
			args:       []string{"-h"},
			want:       0,
			wantStdout: "Usage:",
		},
		{
			name:       "unknown command",
			args:       []string{"frobnicate"},
			want:       2,
			wantStderr: "Usage:",
		},
		{
			name:       "init writes package.json",
			args:       []string{"init"},
			want:       0,
			wantStdout: "Wrote package.json",
		},
		{
			name:           "init rejects extra arguments",
			args:           []string{"init", "extra"},
			want:           2,
			wantStderr:     "init takes no arguments",
			wantNoManifest: true,
		},
		{
			name: "init twice reports the existing file",
			args: []string{"init"},
			setup: func(t *testing.T) {
				if code := run([]string{"init"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
					t.Fatalf("setup init exit code = %d, want 0", code)
				}
			},
			want:       1,
			wantStderr: "package.json already exists",
		},
		{
			name:           "install without package.json",
			args:           []string{"install"},
			want:           1,
			wantStderr:     "no package.json found",
			wantNoManifest: true,
		},
		{
			name: "install of a missing package creates nothing",
			args: []string{"install", "nosuch"},
			setup: func(t *testing.T) {
				t.Setenv("BALE_REGISTRY", notFound.URL)
			},
			want:           1,
			wantStderr:     "nosuch",
			wantNoManifest: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			if tt.setup != nil {
				tt.setup(t)
			}

			var stdout, stderr bytes.Buffer
			got := run(tt.args, &stdout, &stderr)

			if got != tt.want {
				t.Errorf("run(%q) = %d, want %d (stderr: %q)", tt.args, got, tt.want, stderr.String())
			}
			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
			if tt.want == 0 && stderr.Len() != 0 {
				t.Errorf("stderr = %q, want it empty on success", stderr.String())
			}
			if tt.wantNoManifest {
				if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
					t.Error("package.json must not have been created")
				}
			}
		})
	}
}

// TestInitWritesValidManifest checks the contents of a freshly initialized
// package.json, including the slugified project name.
func TestInitWritesValidManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Project")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	if code := run([]string{"init"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run(init) = %d, want 0 (stderr: %q)", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatalf("read package.json: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("package.json is not valid JSON: %v", err)
	}

	m, err := manifest.Load(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if m.Name != "my-project" {
		t.Errorf("name = %q, want %q", m.Name, "my-project")
	}
	if m.Version != "1.0.0" {
		t.Errorf("version = %q, want 1.0.0", m.Version)
	}
	if got := string(m.Extra["main"]); got != `"index.js"` {
		t.Errorf(`Extra["main"] = %s, want "index.js"`, got)
	}
	if got := string(m.Extra["license"]); got != `"ISC"` {
		t.Errorf(`Extra["license"] = %s, want "ISC"`, got)
	}
	var scripts map[string]string
	if err := json.Unmarshal(m.Extra["scripts"], &scripts); err != nil {
		t.Fatalf("unmarshal scripts: %v", err)
	}
	if scripts["test"] == "" {
		t.Error("expected a test script")
	}
	if !strings.Contains(string(data), "&&") {
		t.Errorf("expected literal && in package.json, got:\n%s", data)
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"My Project", "my-project"},
		{"bale", "bale"},
		{"Already-Fine_1.0", "already-fine_1.0"},
		{"  spaced  out  ", "spaced-out"},
		{"...dots...", "dots..."},
		{"@scope/thing", "scope-thing"},
		{"日本語", "package"},
		{"---", "package"},
		{"", "package"},
		{"_private", "private"},
		{strings.Repeat("a", 300), strings.Repeat("a", 214)},
		{strings.Repeat("a ", 200), strings.TrimRight(strings.Repeat("a-", 107)[:214], "-")},
	}

	for _, tt := range tests {
		if got := slugify(tt.in); got != tt.want {
			t.Errorf("slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
