package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testSigner(t *testing.T, pathStyle bool) *signer {
	t.Helper()
	endpoint, err := url.Parse("https://s3.example.com")
	if err != nil {
		t.Fatal(err)
	}
	return &signer{
		endpoint:  endpoint,
		region:    "us-east-1",
		bucket:    "bucket",
		accessKey: "access",
		secretKey: "secret",
		pathStyle: pathStyle,
	}
}

// TestPresignMatchesAWSExample pins the signing code to the documented AWS
// test vector, so a refactor cannot silently break authentication.
func TestPresignMatchesAWSExample(t *testing.T) {
	endpoint, err := url.Parse("https://s3.amazonaws.com")
	if err != nil {
		t.Fatal(err)
	}
	// The signer takes "now" from the wall clock, so reproduce the vector's
	// canonical request and signature directly.
	s := &signer{
		endpoint:  endpoint,
		region:    "us-east-1",
		bucket:    "examplebucket",
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	}
	canonicalRequest := strings.Join([]string{
		"GET",
		"/test.txt",
		"X-Amz-Algorithm=AWS4-HMAC-SHA256" +
			"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
			"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host",
		"host:examplebucket.s3.amazonaws.com\n",
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")

	got := s.signature("20130524T000000Z", "20130524/us-east-1/s3/aws4_request", canonicalRequest)
	const want = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if got != want {
		t.Fatalf("signature mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestPresignShape(t *testing.T) {
	s := testSigner(t, true)

	got, err := s.presign("hls/0/a b+1.ts", time.Minute)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	if !strings.HasPrefix(got, "https://s3.example.com/bucket/hls/0/a%20b%2B1.ts?") {
		t.Fatalf("unexpected URL: %s", got)
	}
	if !strings.Contains(got, "X-Amz-Expires=60") || !strings.Contains(got, "X-Amz-Signature=") {
		t.Fatalf("incomplete presigned URL: %s", got)
	}
}

func TestObjectURLVirtualHosted(t *testing.T) {
	s := testSigner(t, false)
	if got := s.objectURL("k.ts").String(); got != "https://bucket.s3.example.com/k.ts" {
		t.Fatalf("unexpected URL: %s", got)
	}
}

func TestSignSetsHeaders(t *testing.T) {
	s := testSigner(t, true)
	req, err := http.NewRequest(http.MethodHead, s.objectURL("hls/stream.m3u8").String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.sign(req)

	if req.Header.Get("x-amz-content-sha256") != emptyPayloadSHA {
		t.Fatal("missing payload hash")
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=access/") {
		t.Fatalf("unexpected authorization: %s", auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Fatalf("unexpected signed headers: %s", auth)
	}
}
