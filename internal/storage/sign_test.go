package storage

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Published vectors: https://docs.aws.amazon.com/AmazonS3/latest/developerguide/sig-v4-header-based-auth.html
func TestPublishedAWSSignatures(t *testing.T) {
	cfg := Config{Region: "us-east-1", AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, method, path, payloadHash, signature string
		headers                                    map[string]string
	}{
		{"get", "GET", "/test.txt", emptySHA256, "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41", map[string]string{"Range": "bytes=0-9"}},
		{"put", "PUT", "/test$file.text", "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072", "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd", map[string]string{"Date": "Fri, 24 May 2013 00:00:00 GMT", "X-Amz-Storage-Class": "REDUCED_REDUNDANCY"}},
		{"lifecycle", "GET", "/?lifecycle", emptySHA256, "fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543", nil},
		{"list", "GET", "/?max-keys=2&prefix=J", emptySHA256, "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(test.method, "https://examplebucket.s3.amazonaws.com"+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			for key, value := range test.headers {
				req.Header.Set(key, value)
			}
			sign(req, cfg, test.payloadHash, now)
			if !strings.HasSuffix(req.Header.Get("Authorization"), "Signature="+test.signature) {
				t.Fatalf("signature mismatch: %s", req.Header.Get("Authorization"))
			}
		})
	}
}

func TestCanonicalQueryEscapesAndOrdersKeysAndValues(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.com/?a%21=x&a=z&a=+&%C3%A9=%2B&acl", nil)
	if got, want := canonicalQuery(req), "%C3%A9=%2B&a=%20&a=z&a%21=x&acl="; got != want {
		t.Fatalf("canonical query = %q, want %q", got, want)
	}
}
