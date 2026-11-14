package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// corsRules is the CORS configuration shape returned by GetBucketCors. Only the
// fields this service writes are modelled.
type corsRules struct {
	XMLName xml.Name   `xml:"CORSConfiguration"`
	Rules   []corsRule `xml:"CORSRule"`
}

type corsRule struct {
	AllowedOrigins []string `xml:"AllowedOrigin"`
	AllowedMethods []string `xml:"AllowedMethod"`
	AllowedHeaders []string `xml:"AllowedHeader"`
	ExposeHeaders  []string `xml:"ExposeHeader"`
	MaxAgeSeconds  *int     `xml:"MaxAgeSeconds"`
}

// corsOptions describes the rule to write.
type corsOptions struct {
	Origins       []string
	Methods       []string
	AllowedHeader []string
	ExposeHeader  []string
	MaxAge        *int
}

// newCORSOptions builds the default rule from the shared CORS_ALLOWED_ORIGINS
// value, so the bucket always matches what the gateway sends.
func newCORSOptions(cfg *config) corsOptions {
	origins := cfg.corsOrigins
	if len(origins) == 0 {
		origins = []string{"*"}
	}
	maxAge := 3600
	return corsOptions{
		Origins:       origins,
		Methods:       []string{http.MethodGet, http.MethodHead},
		AllowedHeader: []string{"*"},
		ExposeHeader: []string{
			"ETag", "Content-Length", "Content-Range", "Accept-Ranges",
		},
		MaxAge: &maxAge,
	}
}

func (o corsOptions) marshal() ([]byte, error) {
	rules := corsRules{Rules: []corsRule{{
		AllowedOrigins: o.Origins,
		AllowedMethods: o.Methods,
		AllowedHeaders: o.AllowedHeader,
		ExposeHeaders:  o.ExposeHeader,
		MaxAgeSeconds:  o.MaxAge,
	}}}
	body, err := xml.Marshal(rules)
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}

// corsClient talks to the bucket's CORS subresource.
type corsClient struct {
	signer *signer
	client *http.Client
}

func newCORSClient(cfg *config) *corsClient {
	return &corsClient{
		signer: &signer{
			endpoint:  cfg.endpoint,
			region:    cfg.region,
			bucket:    cfg.bucket,
			accessKey: cfg.accessKey,
			secretKey: cfg.secretKey,
			pathStyle: cfg.pathStyle,
		},
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

// corsURL is the bucket URL with the CORS subresource selected.
func (c *corsClient) corsURL() *url.URL {
	u := *c.signer.endpoint
	if c.signer.pathStyle {
		u.Path = "/" + c.signer.bucket
	} else {
		u.Host = c.signer.bucket + "." + u.Host
		u.Path = "/"
	}
	u.RawQuery = "cors="
	return &u
}

// do signs and sends a request against the bucket CORS subresource.
func (c *corsClient) do(method string, body []byte) (*http.Response, []byte, error) {
	u := c.corsURL()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u.String(), reader)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		sum := sha256.Sum256(body)
		c.signer.sign(req, hex.EncodeToString(sum[:]))
		req.Header.Set("Content-Type", "application/xml")
	} else {
		c.signer.sign(req, "")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	return resp, payload, nil
}

// setupCORS applies, deletes or prints the bucket CORS rule.
func runSetupCORS(cfg *config, action string) error {
	client := newCORSClient(cfg)

	switch action {
	case "apply":
		body, err := newCORSOptions(cfg).marshal()
		if err != nil {
			return err
		}
		resp, payload, err := client.do(http.MethodPut, body)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			return s3Error("PutBucketCors", resp.StatusCode, payload)
		}
		fmt.Printf("applied CORS rule to %s/%s\n\n", cfg.endpoint, cfg.bucket)

	case "delete":
		resp, payload, err := client.do(http.MethodDelete, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			return s3Error("DeleteBucketCors", resp.StatusCode, payload)
		}
		fmt.Printf("removed CORS rule from %s/%s\n\n", cfg.endpoint, cfg.bucket)

	case "show":
		// Nothing to change; just read below.

	default:
		return fmt.Errorf("action must be apply, delete or show, got %q", action)
	}

	return printCORSRules(client, cfg)
}

func printCORSRules(client *corsClient, cfg *config) error {
	resp, payload, err := client.do(http.MethodGet, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		fmt.Println("current rule: none configured")
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return s3Error("GetBucketCors", resp.StatusCode, payload)
	}

	var rules corsRules
	if err := xml.Unmarshal(payload, &rules); err != nil {
		return fmt.Errorf("parsing CORS response: %w", err)
	}
	// Providers differ: some answer 404 with NoSuchCORSConfiguration, others
	// return 200 with an empty list. Treat both as "no rule".
	if len(rules.Rules) == 0 {
		fmt.Println("current rule: none configured")
		return nil
	}

	// Print JSON: it is far easier to read than the XML the API returns.
	out, err := json.MarshalIndent(corsRulesJSON(rules), "", "  ")
	if err != nil {
		return err
	}
	fmt.Printf("current rule on %s/%s:\n%s\n", cfg.endpoint, cfg.bucket, out)
	return nil
}

// corsRulesJSON reshapes the XML result into stable, pretty-printed JSON.
func corsRulesJSON(rules corsRules) map[string]any {
	out := make([]map[string]any, 0, len(rules.Rules))
	for _, r := range rules.Rules {
		rule := map[string]any{
			"allowedOrigins": r.AllowedOrigins,
			"allowedMethods": r.AllowedMethods,
			"allowedHeaders": r.AllowedHeaders,
			"exposeHeaders":  r.ExposeHeaders,
		}
		if r.MaxAgeSeconds != nil {
			rule["maxAgeSeconds"] = *r.MaxAgeSeconds
		}
		out = append(out, rule)
	}
	return map[string]any{"corsRules": out}
}

// s3Error turns an S3 error body into a readable Go error.
func s3Error(operation string, status int, payload []byte) error {
	var parsed struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if err := xml.Unmarshal(payload, &parsed); err == nil && parsed.Code != "" {
		return fmt.Errorf("%s: HTTP %d %s: %s", operation, status, parsed.Code, parsed.Message)
	}
	detail := strings.TrimSpace(string(payload))
	if len(detail) > 200 {
		detail = detail[:200] + "..."
	}
	if detail == "" {
		return fmt.Errorf("%s: HTTP %d", operation, status)
	}
	return fmt.Errorf("%s: HTTP %d: %s", operation, status, detail)
}
