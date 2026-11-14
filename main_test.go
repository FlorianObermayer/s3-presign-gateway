package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
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
		// Mirrors the production default.
		proxySuffixes: []string{".m3u8"},
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
	resp := request(testGateway(t, nil), http.MethodGet, "/hls/stream.ts")
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
	resp := requestWithOrigin(g, http.MethodGet, "/hls/stream.ts", "https://streaming.example.com")
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
	resp := requestWithOrigin(g, http.MethodGet, "/hls/stream.ts", "https://evil.example.com")
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
	resp := request(g, http.MethodGet, "/hls/stream.ts")
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
		// Origins are matched exactly, so a hostname wildcard would never
		// match. It must be rejected rather than accepted and silently ignored.
		{name: "hostname wildcard", in: "https://*.example.com", wantErr: true},
		{name: "hostname wildcard in list", in: "https://a.example,https://*.example.com", wantErr: true},
		{name: "bare star still works", in: " * ", want: nil},
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

// TestParseOriginsWildcardMessage keeps the rejection diagnostic helpful: the
// error must say what is wrong, otherwise the operator has no idea why startup
// failed on an origin that looks plausible.
func TestParseOriginsWildcardMessage(t *testing.T) {
	_, err := parseOrigins("https://*.example.com")
	if err == nil {
		t.Fatal("expected wildcard hostname to be rejected")
	}
	for _, want := range []string{"https://*.example.com", "wildcard", "explicitly"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
}

// TestParseOriginsAllowsPort guards against over-eager validation: an Origin
// header includes the port, so a non-default port is a legitimate entry.
func TestParseOriginsAllowsPort(t *testing.T) {
	got, err := parseOrigins("https://a.example:8443")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "https://a.example:8443" {
		t.Fatalf("got %v", got)
	}
}

func TestRedirectIsPresignedAndCORSReadable(t *testing.T) {
	resp := request(testGateway(t, nil), http.MethodGet, "/hls/stream.ts")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "https://s3.example.com/bucket/hls/stream.ts?X-Amz-Signature=") &&
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

func TestTokenRequired(t *testing.T) {
	g := testGateway(t, func(c *config) {
		c.accessTokens = [][]byte{[]byte("s3cr3t-token-value-1234")}
	})

	// Without the token the object is invisible.
	resp := requestWithOrigin(g, http.MethodGet, "/hls/stream.ts", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 without token, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatal("must not redirect without a token")
	}

	// With the token in front of the key it works, and the token is stripped.
	resp = requestWithOrigin(g, http.MethodGet, "/s3cr3t-token-value-1234/hls/stream.ts", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 with token, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "/bucket/hls/stream.ts?") {
		t.Fatalf("token leaked into the key or was not stripped: %s", loc)
	}
}

func TestWrongTokenIsNotFound(t *testing.T) {
	g := testGateway(t, func(c *config) {
		c.accessTokens = [][]byte{[]byte("s3cr3t-token-value-1234")}
	})

	for _, path := range []string{
		"/wrong-token-value-12345/hls/stream.m3u8",
		"/s3cr3t-token-value-123/hls/stream.m3u8", // prefix of the real token
		"/s3cr3t-token-value-1234",
	} {
		resp := request(g, http.MethodGet, path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: expected 404, got %d", path, resp.StatusCode)
		}
	}
}

func TestHealthBypassesToken(t *testing.T) {
	g := testGateway(t, func(c *config) {
		c.accessTokens = [][]byte{[]byte("s3cr3t-token-value-1234")}
	})
	// The container healthcheck must keep working without knowing the token.
	resp := request(g, http.MethodGet, "/healthz")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestTokenRotationAcceptsBoth(t *testing.T) {
	g := testGateway(t, func(c *config) {
		c.accessTokens = [][]byte{
			[]byte("old-token-value-123456"),
			[]byte("new-token-value-123456"),
		}
	})
	for _, token := range []string{"old-token-value-123456", "new-token-value-123456"} {
		resp := request(g, http.MethodGet, "/"+token+"/hls/stream.ts")
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("%s: expected 302, got %d", token, resp.StatusCode)
		}
	}
}

func TestTokenRequiredForPreflight(t *testing.T) {
	g := testGateway(t, func(c *config) {
		c.accessTokens = [][]byte{[]byte("s3cr3t-token-value-1234")}
	})
	resp := request(g, http.MethodOptions, "/hls/0/a.ts")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for tokenless preflight, got %d", resp.StatusCode)
	}
}

func TestParseTokens(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{name: "empty disables the feature", in: "", want: 0},
		{name: "single", in: "abcdefghijklmnop", want: 1},
		{name: "list with spaces", in: " abcdefghijklmnop , qrstuvwxyz123456 ", want: 2},
		{name: "empty entries ignored", in: "abcdefghijklmnop,,", want: 1},
		{name: "too short", in: "tolong", wantErr: true},
		{name: "contains slash", in: "abcdefghij/klmnop", wantErr: true},
		{name: "contains space", in: "abcdefghij klmnop", wantErr: true},
		{name: "contains query marker", in: "abcdefghijklmnop?", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTokens(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d tokens, want %d", len(got), tc.want)
			}
		})
	}
}

// --- proxied objects -------------------------------------------------------

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// upstreamManifest serves a small HLS manifest whose segment URIs are relative,
// the way Owncast writes them.
func upstreamManifest(t *testing.T, method, query *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*method, *query = r.Method, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n"+
			"#EXTINF:2.000,\nstream-abc-1.ts\n#EXTINF:2.000,\nstream-abc-2.ts\n#EXT-X-ENDLIST\n")
	}))
}

func upstreamGateway(t *testing.T, cfg func(*config)) (*gateway, func()) {
	t.Helper()
	var method, query string
	upstream := upstreamManifest(t, &method, &query)

	endpoint, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	g := testGateway(t, func(c *config) {
		c.endpoint = endpoint
		if cfg != nil {
			cfg(c)
		}
	})
	return g, func() { upstream.Close() }
}

// TestManifestIsProxied is the regression test for the bug that a redirect
// leaves the browser on the storage host: relative segment URIs then resolve
// there, unsigned, and every segment is rejected with 403.
func TestManifestIsProxied(t *testing.T) {
	var method, query string
	upstream := upstreamManifest(t, &method, &query)
	defer upstream.Close()

	endpoint, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	g := testGateway(t, func(c *config) { c.endpoint = endpoint })

	resp := request(g, http.MethodGet, "/hls/1/stream.m3u8")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("a manifest must be served, not redirected: %s", loc)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Fatalf("upstream content type not forwarded, got %q", ct)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("proxied responses must not be cached, got %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("missing CORS header, got %q", got)
	}
	if method != http.MethodGet || !strings.Contains(query, "X-Amz-Signature=") {
		t.Fatalf("upstream request was not a presigned GET: %s %q", method, query)
	}

	// The body is forwarded byte for byte. Rewriting it would require knowing
	// the format, which is exactly what this design avoids.
	body := readBody(t, resp)
	if !strings.Contains(body, "\nstream-abc-1.ts") {
		t.Fatalf("body was modified:\n%s", body)
	}
}

// TestRelativeReferenceKeepsToken pins the mechanism the design rests on:
// because the token is a leading path segment, a browser resolving a relative
// reference against the manifest URL lands back on the gateway with the token
// intact. No body rewriting is involved.
func TestRelativeReferenceKeepsToken(t *testing.T) {
	g, done := upstreamGateway(t, func(c *config) {
		c.accessTokens = [][]byte{[]byte("s3cr3t-token-value-1234")}
	})
	defer done()

	manifestPath := "/s3cr3t-token-value-1234/hls/1/stream.m3u8"
	resp := request(g, http.MethodGet, manifestPath)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "stream-abc-1.ts") {
		t.Fatalf("unexpected body:\n%s", body)
	}

	// What the browser does with a relative reference.
	resolved := path.Join(path.Dir(manifestPath), "stream-abc-1.ts")
	if resolved != "/s3cr3t-token-value-1234/hls/1/stream-abc-1.ts" {
		t.Fatalf("unexpected resolution: %s", resolved)
	}

	seg := request(g, http.MethodGet, resolved)
	defer seg.Body.Close()
	if seg.StatusCode != http.StatusFound {
		t.Fatalf("segment via the token path: expected 302, got %d", seg.StatusCode)
	}

	// The token has to stay in the path, so a rewritten body could not help
	// either: only a plain path segment survives relative resolution.
	bare := request(g, http.MethodGet, "/hls/1/stream-abc-1.ts")
	defer bare.Body.Close()
	if bare.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 without the token, got %d", bare.StatusCode)
	}
}

func TestProxyForwardsUpstreamStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such key", http.StatusNotFound)
	}))
	defer upstream.Close()

	endpoint, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	g := testGateway(t, func(c *config) { c.endpoint = endpoint })

	resp := request(g, http.MethodGet, "/hls/missing.m3u8")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected upstream 404, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("must not redirect: %s", loc)
	}
}

// TestProxyForwardsRange covers using the gateway as a plain reverse proxy: a
// large object read in parts still has to answer 206 correctly.
func TestProxyForwardsRange(t *testing.T) {
	var gotRange string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.Header().Set("Content-Range", "bytes 0-3/100")
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "abcd")
	}))
	defer upstream.Close()

	endpoint, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	g := testGateway(t, func(c *config) {
		c.endpoint = endpoint
		c.proxyAll = true
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hls/0/a.ts", nil)
	req.Header.Set("Range", "bytes=0-3")
	g.ServeHTTP(rec, req)
	resp := rec.Result()
	defer resp.Body.Close()

	if gotRange != "bytes=0-3" {
		t.Fatalf("Range not forwarded: %q", gotRange)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 0-3/100" {
		t.Fatalf("Content-Range not forwarded, got %q", got)
	}
	if body := readBody(t, resp); body != "abcd" {
		t.Fatalf("unexpected body %q", body)
	}
}

// TestProxyAllIsFormatAgnostic shows the escape hatch for any manifest format:
// proxy everything and let relative resolution do the work.
func TestProxyAllIsFormatAgnostic(t *testing.T) {
	g, done := upstreamGateway(t, func(c *config) {
		c.proxyAll = true
		c.proxySuffixes = nil
	})
	defer done()

	// Neither a manifest nor a listed suffix: PROXY_ALL must still proxy it.
	resp := request(g, http.MethodGet, "/hls/0/segment.ts")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("must not redirect: %s", loc)
	}
}

func TestProxies(t *testing.T) {
	suffixes, err := parseSuffixes(".m3u8,.mpd, manifest")
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{cfg: &config{proxySuffixes: suffixes}}

	for _, key := range []string{"a/b.m3u8", "a/b.M3U8", "a/b.mpd", "a/b.manifest"} {
		if !g.proxies(key) {
			t.Fatalf("%q should be proxied", key)
		}
	}
	// A suffix is matched against the end of the key, so a longer extension
	// that merely contains it is not enough.
	for _, key := range []string{"a/b.ts", "a/b.m3u8.txt", "a/bm3u8"} {
		if g.proxies(key) {
			t.Fatalf("%q should be redirected", key)
		}
	}
}

func TestParseSuffixes(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{name: "empty disables proxying", in: "", want: nil},
		{name: "single with dot", in: ".m3u8", want: []string{".m3u8"}},
		{name: "dot is optional", in: "m3u8", want: []string{".m3u8"}},
		{name: "case is folded", in: ".M3U8", want: []string{".m3u8"}},
		{name: "list with spaces", in: " .m3u8 , .mpd ", want: []string{".m3u8", ".mpd"}},
		{name: "empty entries ignored", in: ".m3u8,,", want: []string{".m3u8"}},
		{name: "slash rejected", in: "a/b", wantErr: true},
		{name: "space inside rejected", in: "a b", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSuffixes(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.in)
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
