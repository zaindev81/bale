// Command bale manages Node.js versions and npm packages.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"bale/internal/installer"
	"bale/internal/manifest"
	"bale/internal/registry"
)

const usage = `bale - a minimal Node package manager

Usage:
  bale pkg init                Create a new package.json in the current directory
  bale pkg install [pkg...]    Install dependencies from package.json, or add packages
  bale pkg i [pkg...]          Alias for install
  bale pkg list               Show direct dependencies and their installed status
  bale pkg ls                 Alias for list
  bale pkg help                Show this help message

Environment:
  BALE_REGISTRY   Override the npm registry URL (default https://registry.npmjs.org)
`

// maxNameLength is npm's limit on package name length.
const maxNameLength = 214

// manifestMode is the permission a newly created package.json gets. It is
// the mode npm itself uses; a stricter mode would hide the manifest from
// other tools running as a different user.
const manifestMode os.FileMode = 0o644

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// runPackages executes the legacy package manager under bale pkg.
func runPackages(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}

	cmd, rest := args[0], args[1:]

	var err error
	switch cmd {
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	case "init":
		if len(rest) > 0 {
			fmt.Fprintf(stderr, "bale: init takes no arguments\n")
			fmt.Fprint(stderr, usage)
			return 2
		}
		err = runInit(stdout)
	case "install", "i":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = runInstall(ctx, rest, stdout, stderr)
	case "list", "ls":
		if len(rest) == 1 && (rest[0] == "--help" || rest[0] == "-h") {
			fmt.Fprint(stdout, usage)
			return 0
		}
		if len(rest) > 0 {
			fmt.Fprintln(stderr, "bale: list takes no arguments")
			return 2
		}
		dir, cwdErr := os.Getwd()
		if cwdErr != nil {
			err = fmt.Errorf("get working directory: %w", cwdErr)
			break
		}
		in := installer.New(dir, nil)
		in.Out = stdout
		err = in.List()
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}

	if err != nil {
		fmt.Fprintf(stderr, "bale: %v\n", err)
		return 1
	}
	return 0
}

// runInit writes a fresh package.json in the current directory. The file is
// created exclusively, so an existing package.json is reported rather than
// overwritten even if it appears between the check and the write.
func runInit(stdout io.Writer) error {
	const path = "package.json"

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, manifestMode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errors.New("package.json already exists")
		}
		return fmt.Errorf("create package.json: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("create package.json: %w", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("get working directory: %w", err)
	}

	m := &manifest.Manifest{
		Name:    slugify(filepath.Base(cwd)),
		Version: "1.0.0",
	}
	if err := m.SetExtra("main", "index.js"); err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := m.SetExtra("scripts", map[string]string{
		"test": `echo "Error: no test specified" && exit 1`,
	}); err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := m.SetExtra("license", "ISC"); err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := m.Save(path); err != nil {
		_ = os.Remove(path)
		return err
	}
	fmt.Fprintln(stdout, "Wrote package.json")
	return nil
}

// slugify turns a directory name into a name npm would accept: lowercase,
// with every character outside [a-z0-9._-] replaced by "-", runs of "-"
// collapsed, leading ".", "_" and "-" and trailing "-" trimmed, and the
// result capped at npm's 214-character limit.
func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}

	slug := b.String()
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	slug = strings.TrimLeft(slug, "._-")
	slug = strings.TrimRight(slug, "-")
	if len(slug) > maxNameLength {
		slug = strings.TrimRight(slug[:maxNameLength], "-")
	}
	if slug == "" {
		return "package"
	}
	return slug
}

// runInstall installs everything in package.json when specs is empty, or
// resolves and adds each of specs otherwise.
func runInstall(ctx context.Context, specs []string, stdout, stderr io.Writer) error {
	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}

	in := installer.New(dir, registry.NewClient(os.Getenv("BALE_REGISTRY")))
	in.Out = stdout
	in.ErrOut = stderr

	if len(specs) == 0 {
		return in.InstallAll(ctx)
	}
	return in.Add(ctx, specs)
}
