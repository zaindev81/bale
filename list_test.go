package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeListFixture(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestListAndAlias(t *testing.T) {
	for _, command := range []string{"list", "ls"} {
		t.Run(command, func(t *testing.T) {
			t.Chdir(t.TempDir())
			project := `{"dependencies":{"z":"latest","@scope/a":"^1.0.0"},"devDependencies":{"b":"2.0.0"}}`
			writeListFixture(t, "package.json", project)
			writeListFixture(t, "node_modules/@scope/a/package.json", `{"name":"@scope/a","version":"1.2.0"}`)
			writeListFixture(t, "node_modules/z/package.json", `{"name":"z","version":"3.0.0"}`)
			writeListFixture(t, "node_modules/b/package.json", `{"name":"b","version":"2.0.0"}`)
			writeListFixture(t, "node_modules/transitive/package.json", `{"name":"transitive","version":"1.0.0"}`)
			// Listing must work even with an unreachable registry.
			t.Setenv("BALE_REGISTRY", "http://127.0.0.1:1")
			var out, errOut bytes.Buffer
			if code := runPackageCLI([]string{command}, &out, &errOut); code != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, &errOut)
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			want := []string{
				"PACKAGE INSTALLED REQUESTED TYPE STATUS",
				"@scope/a 1.2.0 ^1.0.0 prod ok",
				"z 3.0.0 latest prod tag not checked (offline)",
				"b 2.0.0 2.0.0 dev ok",
			}
			if len(lines) != len(want) {
				t.Fatalf("unexpected output: %s", &out)
			}
			for i, line := range lines {
				if got := strings.Join(strings.Fields(line), " "); got != want[i] {
					t.Errorf("line %d = %q, want %q", i, got, want[i])
				}
			}
			if errOut.Len() != 0 {
				t.Errorf("unexpected stderr: %s", &errOut)
			}
			data, err := os.ReadFile("package.json")
			if err != nil || string(data) != project {
				t.Fatalf("project manifest changed: %q, %v", data, err)
			}
		})
	}
}

func TestListDependencyProblems(t *testing.T) {
	for _, tt := range []struct {
		name, requested, installed, status string
	}{
		{"missing", "^1.0.0", "", "missing"},
		{"mismatch", "^2.0.0", `{"name":"a","version":"1.0.0"}`, "version mismatch"},
		{"malformed manifest", "*", `{`, "invalid package.json"},
		{"missing version", "*", `{"name":"a"}`, "invalid package.json"},
		{"invalid version with tag", "latest", `{"name":"a","version":"broken"}`, "invalid version"},
		{"wrong package", "*", `{"name":"other","version":"1.0.0"}`, "package name mismatch"},
		{"malformed range", "^^1.0.0", `{"name":"a","version":"1.0.0"}`, "invalid range"},
		{"unsupported spec", "file:../a", `{"name":"a","version":"1.0.0"}`, "unsupported spec"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeListFixture(t, "package.json", `{"dependencies":{"a":"`+tt.requested+`","z":"1.0.0"}}`)
			if tt.installed != "" {
				writeListFixture(t, "node_modules/a/package.json", tt.installed)
			}
			writeListFixture(t, "node_modules/z/package.json", `{"name":"z","version":"1.0.0"}`)
			var out, errOut bytes.Buffer
			if code := runPackageCLI([]string{"list"}, &out, &errOut); code != 1 {
				t.Fatalf("exit = %d, want 1; stderr = %q", code, &errOut)
			}
			if !strings.Contains(out.String(), tt.status) {
				t.Errorf("output = %q, want status %q", &out, tt.status)
			}
			if !strings.Contains(strings.Join(strings.Fields(out.String()), " "), "z 1.0.0 1.0.0 prod ok") {
				t.Errorf("healthy dependency omitted: %s", &out)
			}
			if errOut.String() != "bale: found 1 dependency problem\n" {
				t.Errorf("unexpected stderr: %s", &errOut)
			}
		})
	}
}

func TestListEmptyAndInvalidProjects(t *testing.T) {
	for _, tt := range []struct {
		name, project, stdout, stderr string
		args                          []string
		code                          int
	}{
		{name: "empty", project: `{}`, stdout: "No dependencies declared.", code: 0},
		{name: "missing", stderr: "no package.json found", code: 1},
		{name: "malformed", project: `{`, stderr: "list: load manifest", code: 1},
		{name: "unsafe name", project: `{"dependencies":{"../outside":"*"}}`, stderr: "invalid dependency name", code: 1},
		{name: "unsafe dev name", project: `{"devDependencies":{"../outside":"*"}}`, stderr: "invalid dependency name", code: 1},
		{name: "arguments", args: []string{"a"}, stderr: "list takes no arguments", code: 2},
		{name: "unknown flag", args: []string{"--all"}, stderr: "list takes no arguments", code: 2},
		{name: "help", args: []string{"--help"}, stdout: "bale pkg list", code: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if tt.project != "" {
				writeListFixture(t, "package.json", tt.project)
			}
			var out, errOut bytes.Buffer
			if code := runPackageCLI(append([]string{"list"}, tt.args...), &out, &errOut); code != tt.code {
				t.Fatalf("exit = %d, want %d; stderr = %q", code, tt.code, &errOut)
			}
			if !strings.Contains(out.String(), tt.stdout) || !strings.Contains(errOut.String(), tt.stderr) {
				t.Errorf("stdout = %q, stderr = %q", &out, &errOut)
			}
			if _, err := os.Stat("node_modules"); !os.IsNotExist(err) {
				t.Errorf("list created node_modules: %v", err)
			}
		})
	}
}
