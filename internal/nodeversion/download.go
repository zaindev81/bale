package nodeversion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const maxDownload = 200 << 20

func (m *Manager) response(ctx context.Context, name string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.baseURL+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", name, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download %s: HTTP %d", name, resp.StatusCode)
	}
	return resp, nil
}

func (m *Manager) fetch(ctx context.Context, name string, limit int64) ([]byte, error) {
	resp, err := m.response(ctx, name)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds download limit", name)
	}
	return data, nil
}

func checksum(data []byte, filename string) ([]byte, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != filename {
			continue
		}
		sum, err := hex.DecodeString(fields[0])
		if err != nil || len(sum) != sha256.Size {
			return nil, errors.New("invalid SHA-256 checksum")
		}
		return sum, nil
	}
	return nil, fmt.Errorf("no SHA-256 checksum for %s", filename)
}

// Install verifies the official SHA-256 checksum before unpacking into a
// private staging directory. Only complete installations enter versions/.
func (m *Manager) Install(ctx context.Context, selector string) (string, error) {
	if err := validateSelector(selector, true); err != nil {
		return "", err
	}
	// Exact versions that are already installed do not require the network.
	exact := "v" + strings.TrimPrefix(selector, "v")
	if validFull(exact) {
		installed, err := m.Installed()
		if err != nil {
			return "", err
		}
		for _, v := range installed {
			if v == exact {
				return v, nil
			}
		}
	}
	releases, err := m.Remote(ctx, selector)
	if err != nil {
		return "", err
	}
	version := releases[0].Version
	dest := m.versionDir(version)
	if st, err := os.Lstat(dest); err == nil {
		if st.IsDir() && runnable(dest) {
			return version, nil
		}
		return "", fmt.Errorf("installation path %s exists but is incomplete; move it aside before reinstalling", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	archiveRoot := "node-" + version + "-" + m.platform
	filename := archiveRoot + ".tar.gz"
	sums, err := m.fetch(ctx, version+"/SHASUMS256.txt", 1<<20)
	if err != nil {
		return "", err
	}
	expected, err := checksum(sums, filename)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dest), ".install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	archive, err := os.CreateTemp(stage, "archive-")
	if err != nil {
		return "", err
	}
	defer archive.Close()
	resp, err := m.response(ctx, version+"/"+filename)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(resp.Body, maxDownload+1))
	resp.Body.Close()
	if copyErr != nil {
		return "", fmt.Errorf("download Node.js: %w", copyErr)
	}
	if size > maxDownload {
		return "", errors.New("Node.js archive exceeds download limit")
	}
	if hex.EncodeToString(hash.Sum(nil)) != hex.EncodeToString(expected) {
		return "", errors.New("Node.js archive SHA-256 checksum mismatch")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	unpacked := filepath.Join(stage, "node")
	if err := os.Mkdir(unpacked, 0755); err != nil {
		return "", err
	}
	if err := extract(ctx, archive, unpacked, archiveRoot); err != nil {
		return "", fmt.Errorf("unpack Node.js: %w", err)
	}
	if !runnable(unpacked) {
		return "", errors.New("archive is missing executable bin/node")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(unpacked, dest); err != nil {
		// A concurrent installer may have finished the same release first.
		if st, statErr := os.Lstat(dest); statErr == nil && st.IsDir() && runnable(dest) {
			return version, nil
		}
		return "", fmt.Errorf("finish Node.js installation: %w", err)
	}
	return version, nil
}
