package storage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func hashHex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

func mac(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sign(req *http.Request, cfg Config, payloadHash string, now time.Time) {
	now = now.UTC()
	date := now.Format("20060102")
	req.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if cfg.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", cfg.SessionToken)
	}
	canonicalHeaders, signedHeaders := signingHeaders(req)
	uri := strings.ReplaceAll(awsEscape(req.URL.Path), "%2F", "/")
	if uri == "" {
		uri = "/"
	}
	canonical := strings.Join([]string{req.Method, uri, canonicalQuery(req), canonicalHeaders, signedHeaders, payloadHash}, "\n")
	scope := date + "/" + cfg.Region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + req.Header.Get("X-Amz-Date") + "\n" + scope + "\n" + hashHex(canonical)
	key := mac(mac(mac(mac([]byte("AWS4"+cfg.SecretKey), date), cfg.Region), "s3"), "aws4_request")
	signature := hex.EncodeToString(mac(key, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+cfg.AccessKey+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func signingHeaders(req *http.Request) (string, string) {
	headers := map[string]string{"host": req.URL.Host}
	if req.Host != "" {
		headers["host"] = req.Host
	}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if lower == "content-type" || lower == "content-md5" || lower == "date" || lower == "range" || strings.HasPrefix(lower, "x-amz-") {
			normalized := make([]string, len(values))
			for i, value := range values {
				normalized[i] = strings.Join(strings.Fields(value), " ")
			}
			headers[lower] = strings.Join(normalized, ",")
		}
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	var canonical strings.Builder
	for _, name := range names {
		canonical.WriteString(name + ":" + headers[name] + "\n")
	}
	return canonical.String(), strings.Join(names, ";")
}

func canonicalQuery(req *http.Request) string {
	type pair struct{ key, value string }
	var pairs []pair
	for key, values := range req.URL.Query() {
		for _, value := range values {
			pairs = append(pairs, pair{awsEscape(key), awsEscape(value)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].key != pairs[j].key {
			return pairs[i].key < pairs[j].key
		}
		return pairs[i].value < pairs[j].value
	})
	encoded := make([]string, len(pairs))
	for i, p := range pairs {
		encoded[i] = p.key + "=" + p.value
	}
	return strings.Join(encoded, "&")
}

func awsEscape(s string) string {
	const digits = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || strings.ContainsRune("-._~", rune(b)) {
			out.WriteByte(b)
		} else {
			out.WriteByte('%')
			out.WriteByte(digits[b>>4])
			out.WriteByte(digits[b&15])
		}
	}
	return out.String()
}
