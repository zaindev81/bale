package nodeversion

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testManager(t *testing.T, handler http.HandlerFunc) *Manager {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	m, err := New(filepath.Join(t.TempDir(), "bale"))
	if err != nil {
		t.Fatal(err)
	}
	m.baseURL = server.URL
	m.client = server.Client()
	m.platform, m.fileTag = "darwin-arm64", "osx-arm64-tar"
	return m
}

const testIndex = `[
 {"version":"v24.9.0","files":["osx-arm64-tar"],"lts":false},
 {"version":"v22.10.0","files":["osx-arm64-tar"],"lts":"Jod"},
 {"version":"v24.10.1","files":["osx-arm64-tar"],"lts":"Krypton"},
 {"version":"v24.10.0","files":["osx-arm64-tar"],"lts":false},
 {"version":"v99.0.0","files":["linux-x64"],"lts":false},
 {"version":"v26.0.0","files":["osx-arm64-tar"],"lts":false},
 {"version":"v../../escape","files":["osx-arm64-tar"],"lts":false},
 {"version":"v999999999999999999999999.0.0","files":["osx-arm64-tar"],"lts":false}
]`

func TestRemoteSelection(t *testing.T) {
	m := testManager(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, testIndex) })
	for _, tt := range []struct {
		selector string
		want     []string
	}{
		{"24", []string{"v24.10.1", "v24.10.0", "v24.9.0"}},
		{"v24.10", []string{"v24.10.1", "v24.10.0"}},
		{"24.10.0", []string{"v24.10.0"}},
		{"latest", []string{"v26.0.0"}},
		{"lts", []string{"v24.10.1", "v22.10.0"}},
	} {
		releases, err := m.Remote(context.Background(), tt.selector)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range releases {
			got = append(got, r.Version)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.selector, got, tt.want)
		}
	}
	for _, selector := range []string{"2", "99", "24.10.00", "../outside", "v", "24@latest", "-1"} {
		if _, err := m.Remote(context.Background(), selector); err == nil {
			t.Errorf("accepted %q", selector)
		}
	}
}

type archiveEntry struct {
	name, body, link string
	kind             byte
	mode             int64
}

func archiveBytes(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if e.kind == 0 {
			e.kind = tar.TypeReg
		}
		if e.mode == 0 {
			e.mode = 0644
		}
		header := &tar.Header{Name: e.name, Typeflag: e.kind, Linkname: e.link, Mode: e.mode, Size: int64(len(e.body))}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func nodeArchive(t *testing.T) []byte {
	root := "node-v24.10.1-darwin-arm64/"
	return archiveBytes(t, []archiveEntry{
		{name: root + "bin/node", body: "#!/bin/sh\necho v24.10.1\n", mode: 0755},
		// Links may precede their targets in the archive.
		{name: root + "bin/npm", kind: tar.TypeSymlink, link: "../lib/node_modules/npm/bin/npm-cli.js"},
		{name: root + "lib/node_modules/npm/bin/npm-cli.js", body: "#!/bin/sh\necho npm\n", mode: 0755},
	})
}

func serveDistribution(archive []byte, badChecksum bool, failDownload bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			fmt.Fprint(w, testIndex)
		case "/v24.10.1/SHASUMS256.txt":
			sum := sha256.Sum256(archive)
			if badChecksum {
				sum[0] ^= 255
			}
			fmt.Fprintf(w, "%x  node-v24.10.1-darwin-arm64.tar.gz\n", sum)
		case "/v24.10.1/node-v24.10.1-darwin-arm64.tar.gz":
			if failDownload {
				http.Error(w, "unavailable", 503)
				return
			}
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}
}

func TestInstallUseAndOfflineReuse(t *testing.T) {
	m := testManager(t, serveDistribution(nodeArchive(t), false, false))
	v, err := m.Install(context.Background(), "24.10")
	if err != nil || v != "v24.10.1" {
		t.Fatalf("install: %s, %v", v, err)
	}
	if m.Current() != "" {
		t.Fatal("install changed selected version")
	}
	target, err := os.Readlink(filepath.Join(m.versionDir(v), "bin/npm"))
	if err != nil || target != "../lib/node_modules/npm/bin/npm-cli.js" {
		t.Fatalf("npm link: %s, %v", target, err)
	}
	m.baseURL = "http://127.0.0.1:1"
	if _, err := m.Install(context.Background(), "24.10.1"); err != nil {
		t.Fatalf("exact version wasn't reused offline: %v", err)
	}
	if got, err := m.Use("24"); err != nil || got != v {
		t.Fatalf("use: %s, %v", got, err)
	}
	if m.Current() != v {
		t.Fatalf("current = %q", m.Current())
	}
	if _, err := m.Use("22"); err == nil {
		t.Fatal("selected missing version")
	}
	if m.Current() != v {
		t.Fatal("failed use changed selection")
	}
	versions, err := m.Installed()
	if err != nil || !slices.Equal(versions, []string{v}) {
		t.Fatalf("installed: %v, %v", versions, err)
	}
	entries, err := os.ReadDir(filepath.Join(m.Home, "versions"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging files left: %v, %v", entries, err)
	}
}

func TestInstallFailuresLeaveNoVersion(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		corrupt, unavailable bool
		archive              []byte
		want                 string
	}{
		{name: "checksum mismatch", corrupt: true, archive: nodeArchive(t), want: "checksum mismatch"},
		{name: "download failure", unavailable: true, archive: nodeArchive(t), want: "HTTP 503"},
		{name: "invalid gzip", archive: []byte("invalid"), want: "unpack Node.js"},
		{name: "missing node", archive: archiveBytes(t, []archiveEntry{{name: "node-v24.10.1-darwin-arm64/README", body: "readme"}}), want: "missing executable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := testManager(t, serveDistribution(tt.archive, tt.corrupt, tt.unavailable))
			if _, err := m.Install(context.Background(), "24"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("install error = %v, want %s", err, tt.want)
			}
			entries, err := os.ReadDir(filepath.Join(m.Home, "versions"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("partial install remains: %v", entries)
			}
		})
	}
}

func TestArchiveRejectsUnsafeEntries(t *testing.T) {
	root := "node-v24.10.1-darwin-arm64/"
	for _, tt := range []struct {
		name    string
		entries []archiveEntry
	}{
		{"traversal", []archiveEntry{{name: root + "../escape", body: "bad"}}},
		{"absolute", []archiveEntry{{name: "/tmp/escape", body: "bad"}}},
		{"wrong root", []archiveEntry{{name: "other/bin/node", body: "bad"}}},
		{"escaping link", []archiveEntry{{name: root + "bin/npm", kind: tar.TypeSymlink, link: "../../escape"}}},
		{"absolute link", []archiveEntry{{name: root + "bin/npm", kind: tar.TypeSymlink, link: "/tmp/escape"}}},
		{"dangling link", []archiveEntry{{name: root + "bin/npm", kind: tar.TypeSymlink, link: "missing"}}},
		{"hard link", []archiveEntry{{name: root + "bin/npm", kind: tar.TypeLink, link: root + "bin/node"}}},
		{"duplicate", []archiveEntry{{name: root + "bin/node", body: "first"}, {name: root + "bin/node", body: "second"}}},
		{"write through link", []archiveEntry{{name: root + "bin", kind: tar.TypeSymlink, link: "lib"}, {name: root + "bin/node", body: "bad"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := extract(context.Background(), bytes.NewReader(archiveBytes(t, tt.entries)), t.TempDir(), strings.TrimSuffix(root, "/")); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestCancelledInstallAndInvalidIndex(t *testing.T) {
	m := testManager(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "invalid JSON") })
	if _, err := m.Remote(context.Background(), ""); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Install(ctx, "24"); err == nil {
		t.Fatal("cancelled install succeeded")
	}
}

func TestPlatformAndChecksum(t *testing.T) {
	for _, tt := range []struct{ os, arch, platform, tag string }{
		{"darwin", "arm64", "darwin-arm64", "osx-arm64-tar"},
		{"darwin", "amd64", "darwin-x64", "osx-x64-tar"},
		{"linux", "arm64", "linux-arm64", "linux-arm64"},
		{"linux", "amd64", "linux-x64", "linux-x64"},
	} {
		platform, tag, err := platformFor(tt.os, tt.arch)
		if err != nil || platform != tt.platform || tag != tt.tag {
			t.Fatalf("platform: %s %s %v", platform, tag, err)
		}
	}
	if _, _, err := platformFor("windows", "amd64"); err == nil {
		t.Fatal("windows accepted")
	}
	if _, _, err := platformFor("linux", "386"); err == nil {
		t.Fatal("386 accepted")
	}
	for _, data := range []string{"", "abc  node.tar.gz", strings.Repeat("0", 64) + "  other.tar.gz"} {
		if _, err := checksum([]byte(data), "node.tar.gz"); err == nil {
			t.Fatal("missing/invalid checksum accepted")
		}
	}
	var r Release
	if err := json.Unmarshal([]byte(`{"lts":false}`), &r); err != nil || r.LTSName() != "" {
		t.Fatal("false LTS handled incorrectly")
	}
}
