package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Keep the package manager tests exercising its public CLI namespace.
func runPackageCLI(args []string, stdout, stderr *bytes.Buffer) int {
	return run(append([]string{"pkg"}, args...), stdout, stderr)
}

func TestNodeCLI(t *testing.T) {
	t.Setenv("BALE_HOME", filepath.Join(t.TempDir(), "bale"))
	for _, tt := range []struct {
		args     []string
		code     int
		contains string
	}{
		{nil, 0, "Node.js version manager"},
		{[]string{"help"}, 0, "list-remote"},
		{[]string{"install"}, 2, "invalid command or arguments"},
		{[]string{"install", "24", "22"}, 2, "invalid command or arguments"},
		{[]string{"install", "express"}, 1, "invalid Node.js version"},
		{[]string{"list-remote", "24.10.00"}, 1, "invalid Node.js version"},
		{[]string{"use", "24"}, 1, "not installed"},
		{[]string{"list"}, 0, "No Node.js versions installed"},
		{[]string{"ls"}, 0, "No Node.js versions installed"},
		{[]string{"env", "extra"}, 2, "invalid command or arguments"},
	} {
		var out, errOut bytes.Buffer
		code := run(tt.args, &out, &errOut)
		if code != tt.code || !strings.Contains(out.String()+errOut.String(), tt.contains) {
			t.Errorf("run(%v) = %d, %s %s", tt.args, code, &out, &errOut)
		}
	}
}

func TestNodeEnvAndSwitchInShell(t *testing.T) {
	home := filepath.Join(t.TempDir(), "bale ' spaces $d `x`")
	t.Setenv("BALE_HOME", home)
	for _, version := range []string{"v22.1.0", "v24.9.0", "v24.10.0"} {
		bin := filepath.Join(home, "versions", version, "bin")
		if err := os.MkdirAll(bin, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "node"), []byte("#!/bin/sh\nprintf '%s\\n' '"+version+"'\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, selector := range []string{"22", "24", "24.9", "latest"} {
		var out, errOut bytes.Buffer
		if code := run([]string{"use", selector}, &out, &errOut); code != 0 {
			t.Fatalf("use: %s", &errOut)
		}
		out.Reset()
		if code := run([]string{"env"}, &out, &errOut); code != 0 {
			t.Fatalf("env: %s", &errOut)
		}
		script := out.String() + "node --version\n"
		result, err := exec.Command("sh", "-c", script).CombinedOutput()
		expected := map[string]string{"22": "v22.1.0", "24": "v24.10.0", "24.9": "v24.9.0", "latest": "v24.10.0"}[selector]
		if err != nil || strings.TrimSpace(string(result)) != expected {
			t.Fatalf("shell: %s, %v, want %s", result, err, expected)
		}
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"list"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "* v24.10.0") {
		t.Fatalf("list: %s %s", &out, &errOut)
	}
}
