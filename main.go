// Package main serves objects of a private S3-compatible bucket to browsers.
//
// A GET on /<key> answers with a 302 to a freshly presigned URL. The redirect
// carries the CORS headers a browser needs, which the storage provider alone
// cannot supply.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	health := flag.Bool("health-check", false, "probe the local /healthz endpoint and exit")
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

	return cfg, nil
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
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "ETag, Content-Length, Content-Range, Accept-Ranges")

	if r.URL.Path == "/healthz" {
		h.Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
		return
	}

	key, ok := g.objectKey(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
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
	g.signer.sign(req)

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

// objectKey turns a request path into a bucket key, rejecting anything that
// could escape the configured prefix.
func (g *gateway) objectKey(path string) (string, bool) {
	key := strings.TrimPrefix(path, "/")
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
