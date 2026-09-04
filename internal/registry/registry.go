// Package registry implements a minimal HTTP client for the npm registry.
package registry

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBaseURL = "https://registry.npmjs.org"

// userAgent identifies bale to registries.
const userAgent = "bale/0.1 (+https://github.com/zain/bale)"

// maxRedirects caps how many redirects a registry request may follow.
const maxRedirects = 5

// maxTarballSize caps how much of a tarball response body we will read, and
// maxPackumentSize does the same for metadata documents. They are variables
// rather than constants only so that tests can shrink them.
var (
	maxTarballSize   int64 = 256 << 20 // 256 MiB
	maxPackumentSize int64 = 64 << 20  // 64 MiB
)

// Dist describes where and how to fetch a package version's tarball.
type Dist struct {
	Tarball string `json:"tarball"`

	// Integrity is a Subresource Integrity string: a whitespace-separated
	// list of "<alg>-<base64>" entries in any order, for example
	// "sha512-<base64> sha256-<base64>". Only sha256, sha384 and sha512
	// are accepted; see verifyIntegrity.
	Integrity string `json:"integrity"`

	// Shasum is the registry's legacy sha1 digest. It is kept only so that
	// packuments round-trip through this struct; sha1 is not
	// collision-resistant and bale never trusts this field.
	Shasum string `json:"shasum"`
}

// Version describes a single published version of a package.
type Version struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Dependencies map[string]string `json:"dependencies"`
	Dist         Dist              `json:"dist"`
}

// Packument is the registry metadata document for a package: all of its
// published versions plus dist-tags such as "latest".
type Packument struct {
	Name     string             `json:"name"`
	DistTags map[string]string  `json:"dist-tags"`
	Versions map[string]Version `json:"versions"`
}

// Client is a minimal npm registry HTTP client.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	// base is BaseURL parsed once at construction. Every request URL,
	// including tarball URLs and redirect targets, must live on this host.
	base *url.URL
}

// NewClient returns a Client for baseURL. If baseURL is empty or cannot be
// parsed as an absolute URL with a host, the public npm registry is used
// instead, so a Client is always usable.
func NewClient(baseURL string) *Client {
	trimmed := strings.TrimSuffix(baseURL, "/")
	base, err := url.Parse(trimmed)
	if trimmed == "" || err != nil || base.Host == "" {
		trimmed = defaultBaseURL
		base, _ = url.Parse(trimmed)
	}

	// Phase timeouts rather than one overall http.Client.Timeout: a large
	// tarball on a slow link is fine, a registry that stalls is not.
	tr := &http.Transport{}
	if def, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = def.Clone()
	}
	tr.DialContext = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.TLSHandshakeTimeout = 15 * time.Second

	c := &Client{
		BaseURL: trimmed,
		base:    base,
		HTTP: &http.Client{
			// No overall timeout: the caller's context bounds the request.
			Transport: tr,
		},
	}
	c.HTTP.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if !c.onRegistryHost(req.URL) {
			return fmt.Errorf("refusing redirect to %q: not on registry host %s", req.URL.Redacted(), c.base.Host)
		}
		return nil
	}
	return c
}

// onRegistryHost reports whether u satisfies the registry host policy: the
// same host (including port) as the base URL, over either the base URL's
// scheme or https.
func (c *Client) onRegistryHost(u *url.URL) bool {
	return u.Host == c.base.Host && (u.Scheme == c.base.Scheme || u.Scheme == "https")
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Packument fetches the registry metadata document for the package name.
func (c *Client) Packument(ctx context.Context, name string) (*Packument, error) {
	reqURL := c.BaseURL + "/" + encodePackageName(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch packument for %q: %w", name, err)
	}
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch packument for %q: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("package %q not found", name)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch packument for %q: unexpected status %s", name, resp.Status)
	}

	// Read at most one byte past the cap so that an oversized document is
	// detected rather than silently truncated into a parse error.
	counted := &countingReader{r: io.LimitReader(resp.Body, maxPackumentSize+1)}
	var p Packument
	decodeErr := json.NewDecoder(counted).Decode(&p)
	_, _ = io.Copy(io.Discard, counted)
	if counted.n > maxPackumentSize {
		return nil, fmt.Errorf("fetch packument for %q: metadata exceeds %d bytes", name, maxPackumentSize)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("fetch packument for %q: %w", name, decodeErr)
	}

	if p.Name == "" || len(p.Versions) == 0 {
		return nil, fmt.Errorf("fetch packument for %q from %s: response is not a package document", name, reqURL)
	}
	if p.Name != name {
		return nil, fmt.Errorf("registry returned package %q for request %q", p.Name, name)
	}
	return &p, nil
}

// Download fetches a package tarball and verifies its integrity. The tarball
// URL must sit on the registry host; see checkURL.
func (c *Client) Download(ctx context.Context, dist Dist) ([]byte, error) {
	tarballURL, err := url.Parse(dist.Tarball)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", dist.Tarball, err)
	}
	if !c.onRegistryHost(tarballURL) {
		return nil, fmt.Errorf("refusing tarball URL %q: not on registry host %s", dist.Tarball, c.base.Host)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dist.Tarball, nil)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", dist.Tarball, err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", dist.Tarball, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: unexpected status %s", dist.Tarball, resp.Status)
	}
	if resp.ContentLength > maxTarballSize {
		return nil, fmt.Errorf("download %s: tarball exceeds %d bytes", dist.Tarball, maxTarballSize)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTarballSize+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", dist.Tarball, err)
	}
	if int64(len(body)) > maxTarballSize {
		return nil, fmt.Errorf("download %s: tarball exceeds %d bytes", dist.Tarball, maxTarballSize)
	}

	if err := verifyIntegrity(body, dist); err != nil {
		return nil, fmt.Errorf("download %s: %w", dist.Tarball, err)
	}

	return body, nil
}

// errNoIntegrity is returned when a Dist carries no usable integrity hash.
var errNoIntegrity = errors.New("missing or unsupported integrity metadata (need sha256/sha384/sha512)")

// integrityAlgorithms lists the Subresource Integrity algorithms bale
// accepts, strongest first. The digest function and size come from the
// standard library; sha1 is deliberately absent.
var integrityAlgorithms = []struct {
	name string
	size int
	sum  func([]byte) []byte
}{
	{"sha512", sha512.Size, func(b []byte) []byte { s := sha512.Sum512(b); return s[:] }},
	{"sha384", sha512.Size384, func(b []byte) []byte { s := sha512.Sum384(b); return s[:] }},
	{"sha256", sha256.Size, func(b []byte) []byte { s := sha256.Sum256(b); return s[:] }},
}

// verifyIntegrity checks body against dist.Integrity, a Subresource
// Integrity string holding one or more whitespace-separated
// "<alg>-<base64>" entries in any order. The strongest supported algorithm
// present wins (sha512 > sha384 > sha256) and every entry for that
// algorithm must match. Entries naming an unsupported algorithm are
// ignored; if none of the entries is supported, verification fails. The
// legacy sha1 dist.Shasum is never consulted.
func verifyIntegrity(body []byte, dist Dist) error {
	if strings.TrimSpace(dist.Integrity) == "" {
		return errNoIntegrity
	}

	for _, alg := range integrityAlgorithms {
		digests, err := integrityDigests(dist.Integrity, alg.name, alg.size)
		if err != nil {
			return err
		}
		if len(digests) == 0 {
			continue
		}
		got := alg.sum(body)
		for _, want := range digests {
			if subtle.ConstantTimeCompare(got, want) != 1 {
				return fmt.Errorf("%s integrity mismatch", alg.name)
			}
		}
		return nil
	}
	return errNoIntegrity
}

// integrityDigests returns every decoded digest in integrity that names the
// algorithm alg. A malformed entry for alg is an error rather than a
// silently ignored one, so a corrupted hash can never downgrade the check.
func integrityDigests(integrity, alg string, size int) ([][]byte, error) {
	var digests [][]byte
	for _, entry := range strings.Fields(integrity) {
		name, encoded, ok := strings.Cut(entry, "-")
		if !ok || name != alg {
			continue
		}
		// Ignore SRI options ("<base64>?opt=value"), which npm does not use.
		encoded, _, _ = strings.Cut(encoded, "?")
		digest, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("invalid %s integrity: %w", alg, err)
		}
		if len(digest) != size {
			return nil, fmt.Errorf("invalid %s integrity: digest is %d bytes, want %d", alg, len(digest), size)
		}
		digests = append(digests, digest)
	}
	return digests, nil
}

// encodePackageName encodes a package name for use in a registry URL path,
// escaping the slash in scoped names ("@scope/pkg" -> "@scope%2fpkg").
func encodePackageName(name string) string {
	if strings.HasPrefix(name, "@") {
		scope, pkg, ok := strings.Cut(name, "/")
		if ok {
			return url.PathEscape(scope) + "%2f" + url.PathEscape(pkg)
		}
	}
	return url.PathEscape(name)
}
