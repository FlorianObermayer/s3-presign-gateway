package main

import (
	"fmt"
	"io"
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

// --- HLS playlists ---------------------------------------------------------

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// playlistUpstream answers every request with a small VOD playlist whose
// segment URIs are relative, the way Owncast writes them.
func playlistUpstream(t *testing.T, method, query *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*method, *query = r.Method, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n"+
			"#EXTINF:2.000,\nstream-abc-1.ts\n#EXTINF:2.000,\nstream-abc-2.ts\n#EXT-X-ENDLIST\n")
	}))
}

// TestPlaylistIsServedAndRewritten is the regression test for the bug that a
// redirect leaves the browser on the storage host: relative segment URIs then
// resolve there, unsigned, and every segment is rejected with 403.
func TestPlaylistIsServedAndRewritten(t *testing.T) {
	var method, query string
	upstream := playlistUpstream(t, &method, &query)
	defer upstream.Close()

	endpoint, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	g := testGateway(t, func(c *config) {
		c.endpoint = endpoint
		c.accessTokens = [][]byte{[]byte("s3cr3t-token-value-1234")}
	})

	resp := request(g, http.MethodGet, "/s3cr3t-token-value-1234/hls/1/stream.m3u8")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("a playlist must be served, not redirected: %s", loc)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Fatalf("unexpected content type %q", ct)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("playlists must not be cached, got %q", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("missing CORS header, got %q", got)
	}
	if method != http.MethodGet || !strings.Contains(query, "X-Amz-Signature=") {
		t.Fatalf("upstream request was not a presigned GET: %s %q", method, query)
	}

	body := readBody(t, resp)
	// The token must survive: the browser requests these URIs next.
	for _, want := range []string{
		"/s3cr3t-token-value-1234/hls/1/stream-abc-1.ts",
		"/s3cr3t-token-value-1234/hls/1/stream-abc-2.ts",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "\nstream-abc-1.ts") {
		t.Fatalf("relative URI left unrewritten:\n%s", body)
	}
}

func TestPlaylistUpstreamStatusForwarded(t *testing.T) {
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

func TestIsPlaylist(t *testing.T) {
	for _, key := range []string{"a/b.m3u8", "a/b.M3U8"} {
		if !isPlaylist(key) {
			t.Fatalf("%q should be a playlist", key)
		}
	}
	for _, key := range []string{"a/b.ts", "a/b.m3u8.txt", "m3u8"} {
		if isPlaylist(key) {
			t.Fatalf("%q should not be a playlist", key)
		}
	}
}

func TestRewritePlaylist(t *testing.T) {
	g := &gateway{}
	tests := []struct {
		name string
		path string
		in   string
		want string
	}{
		{
			name: "relative segments resolved against the directory",
			path: "/tok/hls/1/stream.m3u8",
			in:   "#EXTM3U\na.ts\nb.ts\n",
			want: "#EXTM3U\n/tok/hls/1/a.ts\n/tok/hls/1/b.ts\n",
		},
		{
			name: "nested relative path",
			path: "/tok/hls/1/stream.m3u8",
			in:   "sub/c.ts\n",
			want: "/tok/hls/1/sub/c.ts\n",
		},
		{
			name: "parent traversal is normalised",
			path: "/tok/hls/1/stream.m3u8",
			in:   "../0/d.ts\n",
			want: "/tok/hls/0/d.ts\n",
		},
		{
			name: "absolute paths are left alone",
			path: "/tok/hls/1/stream.m3u8",
			in:   "/cdn/e.ts\n",
			want: "/cdn/e.ts\n",
		},
		{
			name: "absolute URLs are left alone",
			path: "/tok/hls/1/stream.m3u8",
			in:   "https://cdn.example/f.ts\n",
			want: "https://cdn.example/f.ts\n",
		},
		{
			name: "comments and blank lines are untouched",
			path: "/tok/hls/1/stream.m3u8",
			in:   "#EXTM3U\n\n#EXT-X-ENDLIST\n",
			want: "#EXTM3U\n\n#EXT-X-ENDLIST\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(g.rewritePlaylist([]byte(tc.in), tc.path))
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
