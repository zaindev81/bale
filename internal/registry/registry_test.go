package registry

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// samplePackument is a minimal but non-degenerate packument for name.
func samplePackument(name string) Packument {
	return Packument{
		Name:     name,
		DistTags: map[string]string{"latest": "1.0.0"},
		Versions: map[string]Version{
			"1.0.0": {Name: name, Version: "1.0.0"},
		},
	}
}

func TestPackumentPlainName(t *testing.T) {
	var gotPath, gotAccept, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		gotUA = r.Header.Get("User-Agent")
		json.NewEncoder(w).Encode(samplePackument("foo"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	p, err := c.Packument(context.Background(), "foo")
	if err != nil {
		t.Fatalf("Packument: %v", err)
	}
	if gotPath != "/foo" {
		t.Errorf("path = %q, want /foo", gotPath)
	}
	if gotAccept != "application/vnd.npm.install-v1+json" {
		t.Errorf("Accept header = %q", gotAccept)
	}
	if gotUA != userAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, userAgent)
	}
	if p.Name != "foo" || p.DistTags["latest"] != "1.0.0" {
		t.Errorf("unexpected packument: %+v", p)
	}
}

func TestPackumentScopedName(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RawPath
		if gotPath == "" {
			gotPath = r.URL.Path
		}
		json.NewEncoder(w).Encode(samplePackument("@scope/pkg"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.Packument(context.Background(), "@scope/pkg")
	if err != nil {
		t.Fatalf("Packument: %v", err)
	}
	if gotPath != "/@scope%2fpkg" {
		t.Errorf("path = %q, want /@scope%%2fpkg", gotPath)
	}
}

func TestEncodePackageName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"foo", "foo"},
		{"@scope/pkg", "@scope%2fpkg"},
		{"@a#b/c", "@a%23b%2fc"},
		{"a b", "a%20b"},
		{"@sc ope/p#kg", "@sc%20ope%2fp%23kg"},
	}
	for _, tt := range tests {
		if got := encodePackageName(tt.name); got != tt.want {
			t.Errorf("encodePackageName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestPackumentNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.Packument(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `"nope" not found`) {
		t.Errorf("error = %q, want mention of not found", err)
	}
}

func TestPackumentServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.Packument(context.Background(), "foo")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want mention of status", err)
	}
}

func TestPackumentDegenerateBodies(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"empty object", `{}`, "not a package document"},
		{"null", `null`, "not a package document"},
		{"no versions", `{"name":"foo","versions":{}}`, "not a package document"},
		{"name mismatch", `{"name":"bar","versions":{"1.0.0":{}}}`, `registry returned package "bar" for request "foo"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, tt.body)
			}))
			defer srv.Close()

			c := NewClient(srv.URL)
			_, err := c.Packument(context.Background(), "foo")
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want mention of %q", err, tt.wantErr)
			}
		})
	}
}

func TestPackumentTooLarge(t *testing.T) {
	restore := maxPackumentSize
	maxPackumentSize = 64
	t.Cleanup(func() { maxPackumentSize = restore })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := samplePackument("foo")
		p.DistTags["padding"] = strings.Repeat("x", 512)
		json.NewEncoder(w).Encode(p)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.Packument(context.Background(), "foo")
	if err == nil {
		t.Fatal("expected error for oversized packument")
	}
	if !strings.Contains(err.Error(), "metadata exceeds 64 bytes") {
		t.Errorf("error = %q, want mention of the size cap", err)
	}
}

func TestDownloadGoodSHA512(t *testing.T) {
	body := []byte("tarball contents")
	sum := sha512.Sum512(body)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	dist := Dist{Tarball: srv.URL + "/pkg.tgz", Integrity: integrity}
	got, err := c.Download(context.Background(), dist)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestDownloadBadSHA512(t *testing.T) {
	body := []byte("tarball contents")
	otherSum := sha512.Sum512([]byte("different"))
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(otherSum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	dist := Dist{Tarball: srv.URL + "/pkg.tgz", Integrity: integrity}
	_, err := c.Download(context.Background(), dist)
	if err == nil {
		t.Fatal("expected error for mismatched sha512")
	}
	if !strings.Contains(err.Error(), srv.URL) {
		t.Errorf("error = %q, want mention of tarball URL", err)
	}
}

// TestVerifyIntegrity exercises the Subresource Integrity policy directly.
func TestVerifyIntegrity(t *testing.T) {
	body := []byte("tarball contents")
	other := []byte("different")

	b64 := base64.StdEncoding.EncodeToString
	sha512Of := func(b []byte) string { s := sha512.Sum512(b); return "sha512-" + b64(s[:]) }
	sha384Of := func(b []byte) string { s := sha512.Sum384(b); return "sha384-" + b64(s[:]) }
	sha256Of := func(b []byte) string { s := sha256.Sum256(b); return "sha256-" + b64(s[:]) }

	tests := []struct {
		name      string
		dist      Dist
		wantErr   bool
		errSubstr string
	}{
		{
			name: "canonical sha512",
			dist: Dist{Integrity: sha512Of(body)},
		},
		{
			name: "strongest wins over wrong weaker hash",
			dist: Dist{Integrity: sha256Of(other) + " " + sha512Of(body)},
		},
		{
			name: "sha384 alone",
			dist: Dist{Integrity: sha384Of(body)},
		},
		{
			name: "sha256 alone",
			dist: Dist{Integrity: sha256Of(body)},
		},
		{
			name: "sha512 wins over correct weaker hash order-independently",
			dist: Dist{Integrity: sha512Of(body) + " " + sha256Of(body)},
		},
		{
			name:      "sha512 with wrong digest length",
			dist:      Dist{Integrity: "sha512-" + b64([]byte("too short"))},
			wantErr:   true,
			errSubstr: "invalid sha512 integrity",
		},
		{
			name:      "sha512 with invalid base64",
			dist:      Dist{Integrity: "sha512-!!!not base64!!!"},
			wantErr:   true,
			errSubstr: "invalid sha512 integrity",
		},
		{
			name:      "unsupported algorithm only",
			dist:      Dist{Integrity: "md5-" + b64([]byte("0123456789abcdef"))},
			wantErr:   true,
			errSubstr: "missing or unsupported integrity metadata",
		},
		{
			name:      "sha1 is never accepted",
			dist:      Dist{Integrity: "sha1-" + b64([]byte("0123456789abcdefghij"))},
			wantErr:   true,
			errSubstr: "missing or unsupported integrity metadata",
		},
		{
			name:      "empty integrity with valid shasum",
			dist:      Dist{Shasum: hexSHA1(body)},
			wantErr:   true,
			errSubstr: "missing or unsupported integrity metadata",
		},
		{
			name:      "whitespace-only integrity",
			dist:      Dist{Integrity: "   \t "},
			wantErr:   true,
			errSubstr: "missing or unsupported integrity metadata",
		},
		{
			name:      "wrong sha512",
			dist:      Dist{Integrity: sha512Of(other)},
			wantErr:   true,
			errSubstr: "sha512 integrity mismatch",
		},
		{
			name:      "wrong sha384",
			dist:      Dist{Integrity: sha384Of(other)},
			wantErr:   true,
			errSubstr: "sha384 integrity mismatch",
		},
		{
			name:      "two sha512 entries, one wrong",
			dist:      Dist{Integrity: sha512Of(body) + " " + sha512Of(other)},
			wantErr:   true,
			errSubstr: "sha512 integrity mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyIntegrity(body, tt.dist)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("verifyIntegrity(%+v) = nil, want error", tt.dist)
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("error = %q, want mention of %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("verifyIntegrity(%+v) = %v, want nil", tt.dist, err)
			}
		})
	}
}

func hexSHA1(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

// TestDownloadRejectsSHA1Only proves the legacy shasum field cannot stand in
// for real integrity metadata over the wire.
func TestDownloadRejectsSHA1Only(t *testing.T) {
	body := []byte("tarball contents")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	dist := Dist{Tarball: srv.URL + "/pkg.tgz", Shasum: hexSHA1(body)}
	_, err := c.Download(context.Background(), dist)
	if err == nil {
		t.Fatal("expected error: sha1 shasum is not trusted")
	}
	if !strings.Contains(err.Error(), "missing or unsupported integrity metadata") {
		t.Errorf("error = %q, want mention of missing integrity", err)
	}
}

func TestDownloadNoIntegrity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	dist := Dist{Tarball: srv.URL + "/pkg.tgz"}
	_, err := c.Download(context.Background(), dist)
	if err == nil {
		t.Fatal("expected error for missing integrity data")
	}
	if !strings.Contains(err.Error(), "missing or unsupported integrity metadata") {
		t.Errorf("error = %q, want mention of missing integrity", err)
	}
}

func TestDownloadRefusesOffHostTarball(t *testing.T) {
	var victimHits int
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		victimHits++
		w.Write([]byte("secret"))
	}))
	defer victim.Close()

	registrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("registry should not have been contacted for %s", r.URL)
	}))
	defer registrySrv.Close()

	c := NewClient(registrySrv.URL)
	_, err := c.Download(context.Background(), Dist{Tarball: victim.URL + "/pkg.tgz"})
	if err == nil {
		t.Fatal("expected error for off-host tarball URL")
	}
	if !strings.Contains(err.Error(), "refusing tarball URL") {
		t.Errorf("error = %q, want refusal", err)
	}
	if victimHits != 0 {
		t.Errorf("victim server was contacted %d times, want 0", victimHits)
	}
}

func TestDownloadRefusesRedirectOffHost(t *testing.T) {
	var victimHits int
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		victimHits++
		w.Write([]byte("secret"))
	}))
	defer victim.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, victim.URL+"/pkg.tgz", http.StatusFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.Download(context.Background(), Dist{Tarball: srv.URL + "/pkg.tgz"})
	if err == nil {
		t.Fatal("expected error for off-host redirect")
	}
	if !strings.Contains(err.Error(), "refusing redirect") {
		t.Errorf("error = %q, want redirect refusal", err)
	}
	if victimHits != 0 {
		t.Errorf("victim server was contacted %d times, want 0", victimHits)
	}
}

func TestDownloadFollowsSameHostRedirect(t *testing.T) {
	body := []byte("tarball contents")
	sum := sha512.Sum512(body)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/pkg.tgz", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/blobs/pkg.tgz", http.StatusFound)
	})
	mux.HandleFunc("/blobs/pkg.tgz", func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL)
	got, err := c.Download(context.Background(), Dist{Tarball: srv.URL + "/pkg.tgz", Integrity: integrity})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestDownloadRefusesRedirectLoop(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/pkg.tgz", http.StatusFound)
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.Download(context.Background(), Dist{Tarball: srv.URL + "/pkg.tgz"})
	if err == nil {
		t.Fatal("expected error for redirect loop")
	}
	if !strings.Contains(err.Error(), "stopped after 5 redirects") {
		t.Errorf("error = %q, want redirect cap", err)
	}
}

func TestDownloadTooLarge(t *testing.T) {
	restore := maxTarballSize
	maxTarballSize = 16
	t.Cleanup(func() { maxTarballSize = restore })

	body := []byte(strings.Repeat("x", 64))
	sum := sha512.Sum512(body)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])

	t.Run("known content length", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.Write(body)
		}))
		defer srv.Close()

		c := NewClient(srv.URL)
		_, err := c.Download(context.Background(), Dist{Tarball: srv.URL + "/pkg.tgz", Integrity: integrity})
		if err == nil {
			t.Fatal("expected error for oversized tarball")
		}
		if !strings.Contains(err.Error(), "tarball exceeds 16 bytes") {
			t.Errorf("error = %q, want mention of the size cap", err)
		}
	})

	t.Run("chunked", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			flusher, _ := w.(http.Flusher)
			for i := 0; i < 8; i++ {
				w.Write([]byte("xxxxxxxx"))
				flusher.Flush()
			}
		}))
		defer srv.Close()

		c := NewClient(srv.URL)
		_, err := c.Download(context.Background(), Dist{Tarball: srv.URL + "/pkg.tgz", Integrity: integrity})
		if err == nil {
			t.Fatal("expected error for oversized tarball")
		}
		if !strings.Contains(err.Error(), "tarball exceeds 16 bytes") {
			t.Errorf("error = %q, want mention of the size cap", err)
		}
	})
}

func TestNewClientDefaultBaseURL(t *testing.T) {
	for _, in := range []string{"", "::not a url::", "not-a-url"} {
		c := NewClient(in)
		if c.BaseURL != defaultBaseURL {
			t.Errorf("NewClient(%q).BaseURL = %q, want %q", in, c.BaseURL, defaultBaseURL)
		}
		if c.base == nil || c.base.Host != "registry.npmjs.org" {
			t.Errorf("NewClient(%q) base = %+v, want the default registry host", in, c.base)
		}
	}
}

func TestNewClientTrimsTrailingSlash(t *testing.T) {
	c := NewClient("https://example.test:8443/")
	if c.BaseURL != "https://example.test:8443" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.base.Host != "example.test:8443" {
		t.Errorf("base host = %q, want example.test:8443", c.base.Host)
	}
}

func TestOnRegistryHost(t *testing.T) {
	c := NewClient("http://example.test:8080")
	tests := []struct {
		raw  string
		want bool
	}{
		{"http://example.test:8080/pkg.tgz", true},
		{"https://example.test:8080/pkg.tgz", true}, // upgrade to https is fine
		{"http://example.test/pkg.tgz", false},      // different port
		{"http://evil.test:8080/pkg.tgz", false},
		{"ftp://example.test:8080/pkg.tgz", false},
		{"file:///etc/passwd", false},
	}
	for _, tt := range tests {
		u, err := url.Parse(tt.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", tt.raw, err)
		}
		if got := c.onRegistryHost(u); got != tt.want {
			t.Errorf("onRegistryHost(%q) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}
