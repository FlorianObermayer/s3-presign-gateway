package main

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeS3 records the requests it receives and serves a canned CORS document,
// which lets the tests assert on signing and request shape without a bucket.
// The write request is recorded separately from the readback that follows it.
type fakeS3 struct {
	writeMethod  string
	writeQuery   string
	writeBody    string
	writeAuthz   string
	writePayload string
	getMethod    string
	status       int
	getStatus    int
	response     string
	callCount    int
}

func (f *fakeS3) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.callCount++
		body, _ := io.ReadAll(r.Body)

		if r.Method == http.MethodGet {
			f.getMethod = r.Method
		} else {
			f.writeMethod = r.Method
			f.writeQuery = r.URL.RawQuery
			f.writeBody = string(body)
			f.writeAuthz = r.Header.Get("Authorization")
			f.writePayload = r.Header.Get("x-amz-content-sha256")
		}

		w.Header().Set("Content-Type", "application/xml")
		status := f.status
		if f.status == 0 {
			status = http.StatusOK
		}
		// A readback after an apply or delete usually differs from the write
		// response, so allow overriding it per test.
		if r.Method == http.MethodGet && f.getStatus != 0 {
			status = f.getStatus
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(f.response))
	})
}

func corsTestClient(t *testing.T, f *fakeS3) *corsClient {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)

	endpoint, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return newCORSClient(&config{
		endpoint:  endpoint,
		region:    "us-east-1",
		bucket:    "bucket",
		accessKey: "access",
		secretKey: "secret",
		pathStyle: true,
		ttl:       5 * time.Minute,
	})
}

func TestCORSOptionsMarshal(t *testing.T) {
	maxAge := 600
	body, err := corsOptions{
		Origins:       []string{"https://a.example", "https://b.example"},
		Methods:       []string{"GET", "HEAD"},
		AllowedHeader: []string{"*"},
		ExposeHeader:  []string{"ETag"},
		MaxAge:        &maxAge,
	}.marshal()
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)

	for _, want := range []string{
		"<CORSConfiguration>",
		"<AllowedOrigin>https://a.example</AllowedOrigin>",
		"<AllowedOrigin>https://b.example</AllowedOrigin>",
		"<AllowedMethod>GET</AllowedMethod>",
		"<AllowedMethod>HEAD</AllowedMethod>",
		"<ExposeHeader>ETag</ExposeHeader>",
		"<MaxAgeSeconds>600</MaxAgeSeconds>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in:\n%s", want, got)
		}
	}
}

func TestCORSOptionsOmitsMaxAgeWhenNil(t *testing.T) {
	body, err := corsOptions{
		Origins: []string{"*"},
		Methods: []string{"GET"},
	}.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "MaxAgeSeconds") {
		t.Fatalf("expected no MaxAgeSeconds in:\n%s", body)
	}
}

func TestNewCORSOptionsDefaultsToWildcard(t *testing.T) {
	opts := newCORSOptions(&config{})
	if len(opts.Origins) != 1 || opts.Origins[0] != "*" {
		t.Fatalf("expected wildcard default, got %v", opts.Origins)
	}
	if opts.MaxAge == nil || *opts.MaxAge != 3600 {
		t.Fatalf("unexpected max age: %v", opts.MaxAge)
	}
}

func TestNewCORSOptionsUsesConfiguredOrigins(t *testing.T) {
	opts := newCORSOptions(&config{corsOrigins: []string{"https://a.example"}})
	if len(opts.Origins) != 1 || opts.Origins[0] != "https://a.example" {
		t.Fatalf("unexpected origins: %v", opts.Origins)
	}
}

func TestSetupCORSApplySignsPutRequest(t *testing.T) {
	f := &fakeS3{status: http.StatusOK, response: `<CORSConfiguration/>`}
	client := corsTestClient(t, f)

	err := runSetupCORS(&config{
		endpoint:    client.signer.endpoint,
		region:      "us-east-1",
		bucket:      "bucket",
		accessKey:   "access",
		secretKey:   "secret",
		pathStyle:   true,
		corsOrigins: []string{"https://a.example"},
	}, "apply")
	if err != nil {
		t.Fatalf("runSetupCORS: %v", err)
	}

	if f.writeMethod != http.MethodPut {
		t.Fatalf("expected PUT, got %s", f.writeMethod)
	}
	if f.writeQuery != "cors=" {
		t.Fatalf("expected cors subresource, got %q", f.writeQuery)
	}
	if !strings.Contains(f.writeBody, "<AllowedOrigin>https://a.example</AllowedOrigin>") {
		t.Fatalf("unexpected body: %s", f.writeBody)
	}
	if !strings.HasPrefix(f.writeAuthz, "AWS4-HMAC-SHA256 Credential=access/") {
		t.Fatalf("request was not signed: %q", f.writeAuthz)
	}
	// A body was sent, so the payload hash must not be the empty one.
	if f.writePayload == emptyPayloadSHA {
		t.Fatal("expected body hash, got the empty payload hash")
	}
	// The rule is read back afterwards.
	if f.getMethod != http.MethodGet {
		t.Fatalf("expected a readback GET, got %s", f.getMethod)
	}
}

func TestSetupCORSDeleteSignsDeleteRequest(t *testing.T) {
	f := &fakeS3{status: http.StatusNoContent, getStatus: http.StatusNotFound, response: `<CORSConfiguration/>`}
	client := corsTestClient(t, f)

	err := runSetupCORS(&config{
		endpoint:  client.signer.endpoint,
		region:    "us-east-1",
		bucket:    "bucket",
		accessKey: "access",
		secretKey: "secret",
		pathStyle: true,
	}, "delete")
	if err != nil {
		t.Fatalf("runSetupCORS: %v", err)
	}

	if f.writeMethod != http.MethodDelete {
		t.Fatalf("expected DELETE, got %s", f.writeMethod)
	}
	if f.writePayload != emptyPayloadSHA {
		t.Fatal("expected the empty payload hash for a bodyless request")
	}
	if !strings.Contains(f.writeAuthz, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Fatalf("unexpected signed headers: %q", f.writeAuthz)
	}
}

func TestSetupCORSDeleteAcceptsNoContent(t *testing.T) {
	// Some providers answer 200 instead of 204 for DeleteBucketCors.
	f := &fakeS3{status: http.StatusOK, getStatus: http.StatusNotFound, response: `<CORSConfiguration/>`}
	client := corsTestClient(t, f)

	err := runSetupCORS(&config{
		endpoint:  client.signer.endpoint,
		region:    "us-east-1",
		bucket:    "bucket",
		accessKey: "access",
		secretKey: "secret",
		pathStyle: true,
	}, "delete")
	if err != nil {
		t.Fatalf("runSetupCORS: %v", err)
	}
}

func TestSetupCORSRejectsUnknownAction(t *testing.T) {
	f := &fakeS3{}
	client := corsTestClient(t, f)

	err := runSetupCORS(&config{
		endpoint:  client.signer.endpoint,
		region:    "us-east-1",
		bucket:    "bucket",
		accessKey: "access",
		secretKey: "secret",
		pathStyle: true,
	}, "nope")
	if err == nil {
		t.Fatal("expected an error for an unknown action")
	}
	if f.callCount != 0 {
		t.Fatalf("expected no HTTP calls, got %d", f.callCount)
	}
}

func TestSetupCORSReportsAccessDenied(t *testing.T) {
	f := &fakeS3{
		status: http.StatusForbidden,
		response: `<Error><Code>AccessDenied</Code>` +
			`<Message>Access Denied</Message></Error>`,
	}
	client := corsTestClient(t, f)

	err := runSetupCORS(&config{
		endpoint:  client.signer.endpoint,
		region:    "us-east-1",
		bucket:    "bucket",
		accessKey: "access",
		secretKey: "secret",
		pathStyle: true,
	}, "apply")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"AccessDenied", "403"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
}

func TestSetupCORSShowEmptyConfig(t *testing.T) {
	// Intercolo answers 200 with an empty document rather than 404.
	f := &fakeS3{status: http.StatusOK, response: `<CORSConfiguration/>`}
	client := corsTestClient(t, f)

	err := runSetupCORS(&config{
		endpoint:  client.signer.endpoint,
		region:    "us-east-1",
		bucket:    "bucket",
		accessKey: "access",
		secretKey: "secret",
		pathStyle: true,
	}, "show")
	if err != nil {
		t.Fatalf("runSetupCORS: %v", err)
	}
	if f.getMethod != http.MethodGet {
		t.Fatalf("expected GET, got %s", f.getMethod)
	}
}

func TestSetupCORSShowToleratesMissingConfig(t *testing.T) {
	f := &fakeS3{
		status: http.StatusNotFound,
		response: `<Error><Code>NoSuchCORSConfiguration</Code>` +
			`<Message>The CORS configuration does not exist</Message></Error>`,
	}
	client := corsTestClient(t, f)

	err := runSetupCORS(&config{
		endpoint:  client.signer.endpoint,
		region:    "us-east-1",
		bucket:    "bucket",
		accessKey: "access",
		secretKey: "secret",
		pathStyle: true,
	}, "show")
	if err != nil {
		t.Fatalf("a missing rule is not an error: %v", err)
	}
}

func TestCORSURLVirtualHostedStyle(t *testing.T) {
	// Virtual-hosted style moves the bucket into the host, so it is asserted on
	// the URL rather than by dialling a hostname that cannot resolve.
	endpoint, _ := url.Parse("https://s3.example.com")
	client := newCORSClient(&config{
		endpoint:  endpoint,
		region:    "us-east-1",
		bucket:    "mybucket",
		accessKey: "access",
		secretKey: "secret",
		pathStyle: false,
	})

	got := client.corsURL().String()
	if got != "https://mybucket.s3.example.com/?cors=" {
		t.Fatalf("unexpected URL: %s", got)
	}
}

func TestCORSURLPathStyle(t *testing.T) {
	endpoint, _ := url.Parse("https://s3.example.com")
	client := newCORSClient(&config{
		endpoint:  endpoint,
		region:    "us-east-1",
		bucket:    "mybucket",
		accessKey: "access",
		secretKey: "secret",
		pathStyle: true,
	})

	got := client.corsURL().String()
	if got != "https://s3.example.com/mybucket?cors=" {
		t.Fatalf("unexpected URL: %s", got)
	}
}

func TestS3ErrorFallsBackToBody(t *testing.T) {
	err := s3Error("Op", 500, []byte("plain text failure"))
	if err == nil || !strings.Contains(err.Error(), "plain text failure") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPrintCORSRulesRoundTrips(t *testing.T) {
	// Guards the XML model against field-name drift: whatever the API returns
	// must survive a decode/encode cycle.
	const doc = `<CORSConfiguration><CORSRule>` +
		`<AllowedOrigin>https://a.example</AllowedOrigin>` +
		`<AllowedMethod>GET</AllowedMethod>` +
		`<MaxAgeSeconds>42</MaxAgeSeconds>` +
		`</CORSRule></CORSConfiguration>`

	var rules corsRules
	if err := xml.Unmarshal([]byte(doc), &rules); err != nil {
		t.Fatal(err)
	}
	if len(rules.Rules) != 1 {
		t.Fatalf("expected one rule, got %d", len(rules.Rules))
	}
	rule := rules.Rules[0]
	if rule.AllowedOrigins[0] != "https://a.example" {
		t.Fatalf("unexpected origin: %v", rule.AllowedOrigins)
	}
	if rule.MaxAgeSeconds == nil || *rule.MaxAgeSeconds != 42 {
		t.Fatalf("unexpected max age: %v", rule.MaxAgeSeconds)
	}
}
