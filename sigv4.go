package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	algorithm       = "AWS4-HMAC-SHA256"
	service         = "s3"
	unsignedPayload = "UNSIGNED-PAYLOAD"
	emptyPayloadSHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// signer creates AWS Signature Version 4 URLs for one bucket.
type signer struct {
	endpoint  *url.URL
	region    string
	bucket    string
	accessKey string
	secretKey string
	pathStyle bool
}

// objectURL returns the unsigned URL of an object.
func (s *signer) objectURL(key string) *url.URL {
	u := *s.endpoint
	if s.pathStyle {
		u.Path = "/" + s.bucket + "/" + key
		u.RawPath = "/" + escapePath(s.bucket) + "/" + escapePath(key)
	} else {
		u.Host = s.bucket + "." + u.Host
		u.Path = "/" + key
		u.RawPath = "/" + escapePath(key)
	}
	return &u
}

// presign returns a URL that grants a time limited GET on key.
func (s *signer) presign(key string, ttl time.Duration) (string, error) {
	if key == "" {
		return "", fmt.Errorf("empty key")
	}
	expires := int64(ttl / time.Second)
	if expires < 1 {
		return "", fmt.Errorf("ttl must be at least one second")
	}

	u := s.objectURL(key)
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	scope := date + "/" + s.region + "/" + service + "/aws4_request"

	query := map[string]string{
		"X-Amz-Algorithm":     algorithm,
		"X-Amz-Credential":    s.accessKey + "/" + scope,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       strconv.FormatInt(expires, 10),
		"X-Amz-SignedHeaders": "host",
	}
	canonicalQuery := canonicalQueryString(query)

	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		u.EscapedPath(),
		canonicalQuery,
		"host:" + u.Host + "\n",
		"host",
		unsignedPayload,
	}, "\n")

	signature := s.signature(amzDate, scope, canonicalRequest)

	u.RawQuery = canonicalQuery + "&X-Amz-Signature=" + signature
	return u.String(), nil
}

// sign adds header based authentication to an outbound request. payloadSHA is
// the hex SHA-256 of the request body, or emptyPayloadSHA when there is none.
func (s *signer) sign(req *http.Request, payloadSHA string) {
	if payloadSHA == "" {
		payloadSHA = emptyPayloadSHA
	}
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	scope := date + "/" + s.region + "/" + service + "/aws4_request"

	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadSHA)

	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		canonicalQueryFromRaw(req.URL.RawQuery),
		"host:" + req.URL.Host + "\n" +
			"x-amz-content-sha256:" + payloadSHA + "\n" +
			"x-amz-date:" + amzDate + "\n",
		signedHeaders,
		payloadSHA,
	}, "\n")

	signature := s.signature(amzDate, scope, canonicalRequest)
	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, s.accessKey, scope, signedHeaders, signature,
	))
}

func (s *signer) signature(amzDate, scope, canonicalRequest string) string {
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		hexSHA256(canonicalRequest),
	}, "\n")

	date := scope[:strings.IndexByte(scope, '/')]
	key := hmacSHA256([]byte("AWS4"+s.secretKey), date)
	key = hmacSHA256(key, s.region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	return hex.EncodeToString(hmacSHA256(key, stringToSign))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func hexSHA256(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// canonicalQueryString sorts parameters and encodes them the way SigV4
// requires (RFC 3986, so space is %20 and slash is %2F).
func canonicalQueryString(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, escape(k)+"="+escape(params[k]))
	}
	return strings.Join(parts, "&")
}

// canonicalQueryFromRaw normalises a raw query string for signing. It is empty
// for the object URLs the gateway presigns, but set for bucket subresources.
func canonicalQueryFromRaw(raw string) string {
	if raw == "" {
		return ""
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	params := make(map[string]string, len(values))
	for k, v := range values {
		if len(v) > 0 {
			params[k] = v[0]
		} else {
			params[k] = ""
		}
	}
	return canonicalQueryString(params)
}

// escapePath encodes a key but keeps the slash separators.
func escapePath(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for i := 0; i < len(p); i++ {
		if c := p[i]; isUnreserved(c) || c == '/' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", p[i])
		}
	}
	return b.String()
}

func escape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if isUnreserved(s[i]) {
			b.WriteByte(s[i])
		} else {
			fmt.Fprintf(&b, "%%%02X", s[i])
		}
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
		c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~'
}
