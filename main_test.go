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
