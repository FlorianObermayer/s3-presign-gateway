package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testGateway(t *testing.T, mutate func(*config)) *gateway {
	t.Helper()
	endpoint, err := url.Parse("https://s3.example.com")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{
		addr:      ":0",
		endpoint:  endpoint,
		region:    "us-east-1",
		bucket:    "bucket",
		accessKey: "access",
		secretKey: "secret",
		pathStyle: true,
		ttl:       5 * time.Minute,
		prefix:    "hls",
	}
	if mutate != nil {
		mutate(cfg)
	}
	return newGateway(cfg)
}

func request(g *gateway, method, target string) *http.Response {
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec.Result()
}

func requestWithOrigin(g *gateway, method, target, origin string) *http.Response {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	g.ServeHTTP(rec, req)
	return rec.Result()
}

func TestCORSDefaultsToWildcard(t *testing.T) {
	resp := request(testGateway(t, nil), http.MethodGet, "/hls/stream.m3u8")
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected wildcard, got %q", got)
	}
	if got := resp.Header.Get("Vary"); got != "" {
		t.Fatalf("wildcard must not set Vary, got %q", got)
	}
}

func TestCORSAllowsConfiguredOrigin(t *testing.T) {
	g := testGateway(t, func(c *config) {
		c.corsOrigins = []string{"https://streaming.example.com"}
	})
	resp := requestWithOrigin(g, http.MethodGet, "/hls/stream.m3u8", "https://streaming.example.com")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://streaming.example.com" {
		t.Fatalf("expected echoed origin, got %q", got)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("expected Vary: Origin, got %q", got)
	}
}

func TestCORSRejectsOtherOrigins(t *testing.T) {
	g := testGateway(t, func(c *config) {
		c.corsOrigins = []string{"https://streaming.example.com"}
	})
	resp := requestWithOrigin(g, http.MethodGet, "/hls/stream.m3u8", "https://evil.example.com")
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected no CORS header, got %q", got)
	}
	// The redirect is still issued; the browser blocks it without the header.
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("expected Vary: Origin, got %q", got)
	}
}

func TestCORSIgnoresRequestsWithoutOrigin(t *testing.T) {
	g := testGateway(t, func(c *config) {
		c.corsOrigins = []string{"https://streaming.example.com"}
	})
	// Native players send no Origin and still need the redirect.
	resp := request(g, http.MethodGet, "/hls/stream.m3u8")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("expected no CORS header, got %q", got)
	}
}

func TestParseOrigins(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{name: "wildcard", in: "*", want: nil},
		{name: "empty means wildcard", in: "", want: nil},
		{name: "single", in: "https://a.example", want: []string{"https://a.example"}},
		{name: "multiple with spaces", in: " https://a.example , https://b.example ", want: []string{"https://a.example", "https://b.example"}},
		{name: "trailing slash", in: "https://a.example/", want: []string{"https://a.example"}},
		{name: "duplicates", in: "https://a.example,https://a.example", want: []string{"https://a.example"}},
		{name: "wildcard wins", in: "https://a.example,*", want: nil},
		{name: "missing scheme", in: "a.example", wantErr: true},
		{name: "with path", in: "https://a.example/app", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOrigins(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestRedirectIsPresignedAndCORSReadable(t *testing.T) {
	resp := request(testGateway(t, nil), http.MethodGet, "/hls/stream.m3u8")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "https://s3.example.com/bucket/hls/stream.m3u8?X-Amz-Signature=") &&
		!strings.Contains(loc, "X-Amz-Signature=") {
		t.Fatalf("location is not presigned: %s", loc)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("missing CORS header, got %q", got)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("redirect must not be cached")
	}
}

func TestPreflight(t *testing.T) {
	resp := request(testGateway(t, nil), http.MethodOptions, "/hls/0/a.ts")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "GET") {
		t.Fatalf("unexpected methods: %q", got)
	}
}

func TestKeysOutsidePrefixAreHidden(t *testing.T) {
	g := testGateway(t, nil)
	for _, target := range []string{"/secret/key.pem", "/hls/../secret.key", "/"} {
		resp := request(g, http.MethodGet, target)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: expected 404, got %d", target, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Fatalf("%s: must not redirect", target)
		}
	}
}

func TestHeadIsSignedUpstream(t *testing.T) {
	var method, auth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, auth = r.Method, r.Header.Get("Authorization")
		w.Header().Set("Content-Length", "1234")
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	endpoint, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	g := testGateway(t, func(c *config) { c.endpoint = endpoint })

	resp := request(g, http.MethodHead, "/hls/0/a.ts")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if method != http.MethodHead || !strings.Contains(auth, "AWS4-HMAC-SHA256") {
		t.Fatalf("upstream request was not a signed HEAD: %s %q", method, auth)
	}
	if got := resp.Header.Get("Content-Length"); got != "1234" {
		t.Fatalf("metadata not forwarded, got %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("missing CORS header on HEAD, got %q", got)
	}
}

func TestOtherMethodsRejected(t *testing.T) {
	resp := request(testGateway(t, nil), http.MethodPost, "/hls/a.ts")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", resp.StatusCode)
	}
}

func TestHealth(t *testing.T) {
	resp := request(testGateway(t, nil), http.MethodGet, "/healthz")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
