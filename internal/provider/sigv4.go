package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// SigV4Signer signs requests with AWS Signature Version 4.
//
// It is implemented here rather than pulled in with an SDK because the gateway
// needs exactly one operation against exactly one service, and because in a
// regulated environment a 40-line signer that a reviewer can read end to end
// is easier to get approved than a transitive dependency tree. Credentials
// come from the instance role in a deployed environment; the static fields
// exist for local testing against a mock endpoint.
type SigV4Signer struct {
	Region          string
	Service         string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

const (
	sigAlgorithm  = "AWS4-HMAC-SHA256"
	sigDateFormat = "20060102T150405Z"
	sigDayFormat  = "20060102"
)

// Sign adds the Authorization and x-amz-* headers to req for the given body.
func (s *SigV4Signer) Sign(req *http.Request, body []byte, now time.Time) error {
	if s.AccessKeyID == "" || s.SecretAccessKey == "" {
		return fmt.Errorf("sigv4: credentials are not configured")
	}
	amzDate := now.Format(sigDateFormat)
	dateStamp := now.Format(sigDayFormat)

	payloadHash := hex.EncodeToString(sha256Sum(body))
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if s.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.SessionToken)
	}
	if req.Host != "" {
		req.Header.Set("Host", req.Host)
	} else {
		req.Header.Set("Host", req.URL.Host)
	}

	signedHeaders, canonicalHeaders := canonicalHeaderSet(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.EscapedPath()),
		canonicalQuery(req.URL.RawQuery),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, s.Region, s.Service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		sigAlgorithm,
		amzDate,
		scope,
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest))),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+s.SecretAccessKey), dateStamp)
	kRegion := hmacSHA256(kDate, s.Region)
	kService := hmacSHA256(kRegion, s.Service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		sigAlgorithm, s.AccessKeyID, scope, signedHeaders, signature))
	return nil
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalHeaderSet builds the signed header list and the canonical header
// block. Only the headers that must be signed are included: signing every
// header makes the signature brittle against proxies that add their own.
func canonicalHeaderSet(req *http.Request) (string, string) {
	include := map[string]bool{"host": true, "content-type": true}
	names := []string{}
	values := map[string]string{}
	for name, vs := range req.Header {
		lower := strings.ToLower(name)
		if !include[lower] && !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		if lower == "authorization" {
			continue
		}
		names = append(names, lower)
		trimmed := make([]string, 0, len(vs))
		for _, v := range vs {
			trimmed = append(trimmed, strings.Join(strings.Fields(v), " "))
		}
		values[lower] = strings.Join(trimmed, ",")
	}
	if _, ok := values["host"]; !ok {
		names = append(names, "host")
		values["host"] = req.URL.Host
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(values[n])
		b.WriteByte('\n')
	}
	return strings.Join(names, ";"), b.String()
}

func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	sort.Strings(parts)
	return strings.Join(parts, "&")
}
