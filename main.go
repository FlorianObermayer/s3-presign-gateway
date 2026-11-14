// Package main serves objects of a private S3-compatible bucket to browsers.
//
// A GET on /<key> answers with a 302 to a freshly presigned URL. The redirect
// carries the CORS headers a browser needs, which the storage provider alone
// cannot supply.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	health := flag.Bool("health-check", false, "probe the local /healthz endpoint and exit")
	setupCORS := flag.Bool("setup-cors", false, "apply, delete or print the bucket CORS rule, then exit")
	// ACTION keeps the compose service declarative; the flag still wins.
	action := flag.String("action", env("ACTION", "apply"), "with -setup-cors: apply, delete or show")
	flag.Parse()

	if *showVersion {
		fmt.Println("s3-presign-gateway", version)
		return
	}
	if *health {
		if err := probe(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *setupCORS {
		cfg, err := loadConfig()
		if err != nil {
			log.Fatalf("configuration: %v", err)
		}
		if err := runSetupCORS(cfg, *action); err != nil {
			log.Fatalf("setup-cors: %v", err)
		}
		return
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           newGateway(cfg),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on %s: bucket %s at %s, prefix %q, ttl %s",
			cfg.addr, cfg.bucket, cfg.endpoint, cfg.prefix, cfg.ttl)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func probe() error {
	addr := env("LISTEN_ADDR", ":8080")
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check: status %d", resp.StatusCode)
	}
	return nil
}

// --- configuration ---------------------------------------------------------

type config struct {
	addr      string
	endpoint  *url.URL
	region    string
	bucket    string
	accessKey string
	secretKey string
	pathStyle bool
	ttl       time.Duration
	prefix    string
	// corsOrigins is empty when every origin is allowed.
	corsOrigins []string
	// accessTokens is empty when no token is required. Each entry is a secret
	// path prefix that must match the first URL segment.
	accessTokens [][]byte
}

func loadConfig() (*config, error) {
	cfg := &config{
		addr:      env("LISTEN_ADDR", ":8080"),
		region:    env("S3_REGION", "us-east-1"),
		bucket:    strings.TrimSpace(os.Getenv("S3_BUCKET")),
		accessKey: secret("S3_ACCESS_KEY_ID"),
		secretKey: secret("S3_SECRET_ACCESS_KEY"),
		prefix:    strings.Trim(os.Getenv("ALLOW_PREFIX"), "/"),
		pathStyle: true,
		ttl:       300 * time.Second,
	}

	raw := env("S3_ENDPOINT", "")
	if raw == "" {
		return nil, errors.New("S3_ENDPOINT is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Host == "" {
		return nil, fmt.Errorf("S3_ENDPOINT is not a valid URL: %q", raw)
	}
	endpoint.Path = ""
	cfg.endpoint = endpoint

	if cfg.bucket == "" {
		return nil, errors.New("S3_BUCKET is required")
	}
	if cfg.accessKey == "" || cfg.secretKey == "" {
		return nil, errors.New("S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY are required")
	}

	if v := os.Getenv("S3_PATH_STYLE"); v != "" {
		cfg.pathStyle, err = strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("S3_PATH_STYLE: %w", err)
		}
	}

	if v := os.Getenv("PRESIGN_TTL_SECONDS"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs < 1 || secs > 604800 {
			return nil, errors.New("PRESIGN_TTL_SECONDS must be between 1 and 604800")
		}
		cfg.ttl = time.Duration(secs) * time.Second
	}

	origins, err := parseOrigins(env("CORS_ALLOWED_ORIGINS", "*"))
	if err != nil {
		return nil, err
	}
	cfg.corsOrigins = origins

	tokens, err := parseTokens(os.Getenv("ACCESS_TOKENS"))
	if err != nil {
		return nil, err
	}
	cfg.accessTokens = tokens

	return cfg, nil
}

// minTokenLength keeps a shared secret out of brute-force range. 16 characters
// of a random alphabet is already far beyond what guessing can reach, but a
// short human-typed word is not.
const minTokenLength = 16

// parseTokens reads a comma separated list so a token can be rotated without
// downtime: add the new one, switch Owncast over, then drop the old one.
func parseTokens(raw string) ([][]byte, error) {
	var tokens [][]byte
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if len(entry) < minTokenLength {
			return nil, fmt.Errorf("ACCESS_TOKENS entry must be at least %d characters", minTokenLength)
		}
		if strings.ContainsAny(entry, "/\\?# ") {
			return nil, fmt.Errorf("ACCESS_TOKENS entry %q must not contain slashes, whitespace or URL punctuation", entry)
		}
		tokens = append(tokens, []byte(entry))
	}
	return tokens, nil
}

// parseOrigins turns a comma separated origin list into a slice. "*" yields
// nil, which means every origin is permitted. Entries must be absolute
// http(s) origins, so a typo fails at startup instead of silently at runtime.
func parseOrigins(raw string) ([]string, error) {
	var origins []string
	seen := make(map[string]struct{})

	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimRight(strings.TrimSpace(entry), "/")
		if entry == "" {
			continue
		}
		if entry == "*" {
			return nil, nil
		}

		u, err := url.Parse(entry)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS entry %q must look like https://host", entry)
		}
		if u.RawQuery != "" || u.Path != "" || u.User != nil {
			return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS entry %q must be an origin without a path", entry)
		}
		// Origins are matched exactly, so a wildcard can never match a real
		// Origin header. Reject it instead of failing silently at runtime:
		// enumerate the subdomains, or use "*" on its own.
		if strings.Contains(u.Host, "*") {
			return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS entry %q uses a wildcard, which can never match; list the origins explicitly or use *", entry)
		}

		if _, ok := seen[entry]; ok {
			continue
		}
		seen[entry] = struct{}{}
		origins = append(origins, entry)
	}

	if len(origins) == 0 {
		return nil, nil
	}
	return origins, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// secret accepts an inline value or a *_FILE path, which is how Docker
// secrets are usually provided.
func secret(key string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimSpace(v)
	}
	if p := os.Getenv(key + "_FILE"); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

// --- HTTP ------------------------------------------------------------------

type gateway struct {
	cfg    *config
	signer *signer
	client *http.Client
}

func newGateway(cfg *config) *gateway {
	return &gateway{
		cfg: cfg,
		signer: &signer{
			endpoint:  cfg.endpoint,
			region:    cfg.region,
			bucket:    cfg.bucket,
			accessKey: cfg.accessKey,
			secretKey: cfg.secretKey,
			pathStyle: cfg.pathStyle,
		},
		client: &http.Client{
			Timeout: 10 * time.Second,
			// A signature is bound to its request; never follow redirects.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	if origin := g.allowedOrigin(r.Header.Get("Origin")); origin != "" {
		h.Set("Access-Control-Allow-Origin", origin)
	}
	// With a fixed origin list the response depends on the request's Origin,
	// so caches must not reuse it across origins.
	if len(g.cfg.corsOrigins) > 0 {
		h.Add("Vary", "Origin")
	}
	h.Set("Access-Control-Expose-Headers", "ETag, Content-Length, Content-Range, Accept-Ranges")

	if r.URL.Path == "/healthz" {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
		return
	}

	path, ok := g.stripToken(r.URL.Path)
	if !ok {
		// Deliberately 404 rather than 403: a wrong token should be
		// indistinguishable from a path that does not exist.
		http.NotFound(w, r)
		return
	}

	key, ok := g.objectKey(path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		// A playlist is served, not redirected: the browser resolves the
		// relative segment URIs against the URL that served the playlist, so
		// after a redirect they would hit the storage host without a signature.
		if isPlaylist(key) {
			g.servePlaylist(w, r, key)
			return
		}
		g.redirect(w, key)
	case http.MethodHead:
		g.head(w, r, key)
	case http.MethodOptions:
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Range, If-None-Match, If-Modified-Since")
		h.Set("Access-Control-Max-Age", "3600")
		w.WriteHeader(http.StatusNoContent)
	default:
		h.Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// allowedOrigin returns the Access-Control-Allow-Origin value for the request's
// Origin, or "" when it is not permitted. The browser sends the page's origin on
// both the gateway request and the presigned request, so this should list the
// same origins as the bucket CORS rule.
func (g *gateway) allowedOrigin(origin string) string {
	if len(g.cfg.corsOrigins) == 0 {
		return "*"
	}
	if origin == "" {
		return ""
	}
	origin = strings.TrimRight(origin, "/")
	for _, allowed := range g.cfg.corsOrigins {
		if strings.EqualFold(allowed, origin) {
			return origin
		}
	}
	return ""
}

func (g *gateway) redirect(w http.ResponseWriter, key string) {
	presigned, err := g.signer.presign(key, g.cfg.ttl)
	if err != nil {
		log.Printf("presign %q: %v", key, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Location", presigned)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusFound)
}

// head answers metadata itself. A HEAD against a URL signed for GET is
// rejected by S3 providers, because the method is part of the signature.
func (g *gateway) head(w http.ResponseWriter, r *http.Request, key string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodHead,
		g.signer.objectURL(key).String(), nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	g.signer.sign(req, "")

	resp, err := g.client.Do(req)
	if err != nil {
		log.Printf("head %q: %v", key, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for _, name := range []string{"Content-Length", "Content-Type", "ETag", "Last-Modified", "Accept-Ranges"} {
		if v := resp.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
}

// isPlaylist reports whether key is an HLS playlist that needs its relative
// URIs rewritten.
func isPlaylist(key string) bool {
	return strings.HasSuffix(strings.ToLower(key), ".m3u8")
}

// servePlaylist fetches an HLS playlist and rewrites it so every relative URI
// points back at this gateway instead of at the storage host.
//
// Redirecting a playlist does not work: the browser resolves the relative
// segment URIs against the URL it fetched the playlist from, which after the
// redirect is the provider, and those requests carry no signature. Serving the
// playlist itself keeps the browser on the gateway for the next hop too.
func (g *gateway) servePlaylist(w http.ResponseWriter, r *http.Request, key string) {
	presigned, err := g.signer.presign(key, g.cfg.ttl)
	if err != nil {
		log.Printf("presign %q: %v", key, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, presigned, nil)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	resp, err := g.client.Do(req)
	if err != nil {
		log.Printf("fetch playlist %q: %v", key, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Printf("read playlist %q: %v", key, err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}

	rewritten := g.rewritePlaylist(body, r.URL.Path)

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Content-Length", strconv.Itoa(len(rewritten)))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(rewritten)
}

// rewritePlaylist turns relative playlist URIs into paths on this gateway.
// Absolute URIs and comments are left alone, so a playlist that already points
// somewhere (a CDN, say) keeps working.
//
// requestPath must still carry the access token: the rewritten URIs are
// requested by the browser next, and without the token they would 404.
func (g *gateway) rewritePlaylist(body []byte, requestPath string) []byte {
	base := "/"
	if i := strings.LastIndex(requestPath, "/"); i >= 0 {
		base = requestPath[:i+1]
	}

	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "://") || strings.HasPrefix(trimmed, "/") {
			continue
		}
		// Resolve against the playlist's directory, normalising any ../.
		lines[i] = path.Join(base, trimmed)
	}
	return []byte(strings.Join(lines, "\n"))
}

// stripToken removes a leading access token from the request path.
// Without configured tokens the path is returned unchanged, so the feature is
// purely opt-in. The comparison is constant time to avoid leaking a prefix
// match through timing.
func (g *gateway) stripToken(path string) (string, bool) {
	if len(g.cfg.accessTokens) == 0 {
		return path, true
	}

	rest := strings.TrimPrefix(path, "/")
	segment, tail, _ := strings.Cut(rest, "/")
	candidate := []byte(segment)

	matched := false
	for _, token := range g.cfg.accessTokens {
		// ConstantTimeCompare returns 0 on differing lengths, and it must run
		// for every token regardless of an earlier match.
		if subtle.ConstantTimeCompare(candidate, token) == 1 {
			matched = true
		}
	}
	if !matched {
		return "", false
	}
	return "/" + tail, true
}

// objectKey turns a request path into a bucket key, rejecting anything that
// could escape the configured prefix.
func (g *gateway) objectKey(requestPath string) (string, bool) {
	key := strings.TrimPrefix(requestPath, "/")
	if key == "" || len(key) > 1024 {
		return "", false
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", false
		}
	}
	if strings.ContainsAny(key, "\\\x00") {
		return "", false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] == 0x7f {
			return "", false
		}
	}
	if g.cfg.prefix != "" && key != g.cfg.prefix && !strings.HasPrefix(key, g.cfg.prefix+"/") {
		return "", false
	}
	return key, true
}
