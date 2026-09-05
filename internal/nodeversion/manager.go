// Package nodeversion installs official Node.js distributions and selects a
// version through a stable current/bin path. It never edits shell startup files.
package nodeversion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

const distributionURL = "https://nodejs.org/dist"

var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*)){0,2}$`)

// Manager stores versions for the current operating system and architecture.
// The network fields are private so production downloads always use nodejs.org.
type Manager struct {
	Home     string
	baseURL  string
	client   *http.Client
	platform string
	fileTag  string
}

type Release struct {
	Version string          `json:"version"`
	Files   []string        `json:"files"`
	LTS     json.RawMessage `json:"lts"`
}

func (r Release) LTSName() string {
	var name string
	_ = json.Unmarshal(r.LTS, &name)
	return name
}

func New(home string) (*Manager, error) {
	platform, tag, err := platformFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(userHome, ".bale")
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	return &Manager{Home: home, baseURL: distributionURL, platform: platform, fileTag: tag,
		client: &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many download redirects")
			}
			if req.URL.Scheme != "https" || req.URL.Host != "nodejs.org" {
				return errors.New("download redirect outside nodejs.org refused")
			}
			return nil
		}},
	}, nil
}

func platformFor(goos, arch string) (string, string, error) {
	switch arch {
	case "amd64":
		arch = "x64"
	case "arm64":
	default:
		return "", "", fmt.Errorf("unsupported architecture %s (supported: amd64, arm64)", arch)
	}
	switch goos {
	case "darwin":
		return "darwin-" + arch, "osx-" + arch + "-tar", nil
	case "linux":
		return "linux-" + arch, "linux-" + arch, nil
	default:
		return "", "", fmt.Errorf("unsupported OS %s (supported: macOS, Linux)", goos)
	}
}

func validateSelector(s string, lts bool) error {
	if s == "latest" || (lts && s == "lts") || versionPattern.MatchString(s) {
		return nil
	}
	aliases := "latest"
	if lts {
		aliases += ", or lts"
	}
	return fmt.Errorf("invalid Node.js version %q; use a major, major.minor, full version, or %s", s, aliases)
}

func matches(version, selector string) bool {
	version = strings.TrimPrefix(version, "v")
	selector = strings.TrimPrefix(selector, "v")
	return selector == "" || selector == "latest" || selector == "lts" || version == selector || strings.HasPrefix(version, selector+".")
}

func validFull(version string) bool {
	if !strings.HasPrefix(version, "v") || !versionPattern.MatchString(version) {
		return false
	}
	_, err := semver.StrictNewVersion(strings.TrimPrefix(version, "v"))
	return err == nil
}

func newer(a, b string) int {
	av, _ := semver.StrictNewVersion(strings.TrimPrefix(a, "v"))
	bv, _ := semver.StrictNewVersion(strings.TrimPrefix(b, "v"))
	return bv.Compare(av)
}

// Remote lists matching releases that have an archive for this platform.
func (m *Manager) Remote(ctx context.Context, selector string) ([]Release, error) {
	if selector != "" {
		if err := validateSelector(selector, true); err != nil {
			return nil, err
		}
	}
	data, err := m.fetch(ctx, "index.json", 8<<20)
	if err != nil {
		return nil, err
	}
	var releases []Release
	if err := json.Unmarshal(data, &releases); err != nil {
		return nil, fmt.Errorf("read Node.js version index: %w", err)
	}
	result := make([]Release, 0)
	for _, r := range releases {
		if validFull(r.Version) && slices.Contains(r.Files, m.fileTag) && matches(r.Version, selector) && (selector != "lts" || r.LTSName() != "") {
			result = append(result, r)
		}
	}
	slices.SortFunc(result, func(a, b Release) int { return newer(a.Version, b.Version) })
	if len(result) == 0 {
		return nil, fmt.Errorf("no Node.js release matches %q for %s; run bale list-remote", selector, m.platform)
	}
	if selector == "latest" {
		result = result[:1]
	}
	return result, nil
}

func (m *Manager) versionDir(version string) string {
	return filepath.Join(m.Home, "versions", version)
}

func runnable(dir string) bool {
	st, err := os.Lstat(filepath.Join(dir, "bin", "node"))
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0111 != 0
}

// Installed is local-only and excludes incomplete installations.
func (m *Manager) Installed() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(m.Home, "versions"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && validFull(e.Name()) && runnable(m.versionDir(e.Name())) {
			versions = append(versions, e.Name())
		}
	}
	slices.SortFunc(versions, newer)
	return versions, nil
}

func (m *Manager) Current() string {
	target, err := os.Readlink(filepath.Join(m.Home, "current"))
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(m.Home, target)
	}
	version := filepath.Base(target)
	if !validFull(version) || filepath.Clean(target) != m.versionDir(version) {
		return ""
	}
	return version
}

// Use atomically updates a shared symlink. A child process cannot change its
// parent's environment; the shell must put current/bin on PATH with bale env.
func (m *Manager) Use(selector string) (string, error) {
	if err := validateSelector(selector, false); err != nil {
		return "", err
	}
	installed, err := m.Installed()
	if err != nil {
		return "", err
	}
	for _, v := range installed {
		if !matches(v, selector) {
			continue
		}
		temp, err := os.MkdirTemp(m.Home, ".select-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(temp)
		link := filepath.Join(temp, "current")
		if err := os.Symlink(m.versionDir(v), link); err != nil {
			return "", err
		}
		if err := os.Rename(link, filepath.Join(m.Home, "current")); err != nil {
			return "", fmt.Errorf("select Node.js: %w", err)
		}
		return v, nil
	}
	return "", fmt.Errorf("Node.js %s is not installed; run bale install %s first", selector, selector)
}
