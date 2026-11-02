package updates

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCheckChannelsAndSemVerOrdering(t *testing.T) {
	f := newFixture(t)
	for _, v := range []string{"1.0.1", "0.9.9", "2.0.0-beta.2", "1.0.0+other-build", "9.0.0", "1.10.0", "1.9.0"} {
		f.github.add(t, v, 1)
	}
	f.github.release("9.0.0").Draft = true
	f.github.releases = append(f.github.releases, githubRelease{Tag: "v01.0.0"})
	for c, want := range map[Channel]string{"": "1.10.0", Stable: "1.10.0", Beta: "2.0.0-beta.2"} {
		got, err := f.m.Check(context.Background(), c)
		must(t, err)
		if got.Available == nil || got.Available.Version != want {
			t.Fatalf("%s: got %+v, want %s", c, got, want)
		}
	}
	if _, err := f.m.Check(context.Background(), "nightly"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestCheckNoNewerReleaseAndPagination(t *testing.T) {
	f := newFixture(t)
	f.github.add(t, "0.9.0", 1)
	f.github.add(t, "1.0.0+metadata", 1)
	got, err := f.m.Check(context.Background(), Stable)
	must(t, err)
	if got.Available != nil {
		t.Fatal("older or equal-precedence release offered")
	}
	for i := 0; i < 100; i++ {
		f.github.releases = append(f.github.releases, githubRelease{Tag: fmt.Sprintf("v0.0.%d", i)})
	}
	f.github.add(t, "1.1.0", 1)
	got, err = f.m.Check(context.Background(), Stable)
	must(t, err)
	if got.Available == nil || got.Available.Version != "1.1.0" {
		t.Fatal("new release on a later page was missed")
	}
}

func TestCheckFailsClosedOnUnsignedNewestAndAmbiguousBuilds(t *testing.T) {
	for _, mode := range []string{"unsigned newest", "ambiguous precedence"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.github.add(t, "1.1.0", 1)
			if mode == "unsigned newest" {
				f.github.add(t, "1.2.0", 1)
				f.github.release("1.2.0").Assets = nil
			} else {
				f.github.add(t, "1.1.0+other", 1)
			}
			if _, err := f.m.Check(context.Background(), Stable); !errors.Is(err, ErrVerification) {
				t.Fatalf("unsafe fallback: %v", err)
			}
		})
	}
}

func TestStageRejectsVersionPolicyAndSchema(t *testing.T) {
	for _, tc := range []struct {
		version string
		channel Channel
		schema  int
		draft   bool
		kind    error
	}{
		{"0.9.0", Stable, 1, false, ErrConflict},
		{"1.0.0+other", Stable, 1, false, ErrConflict},
		{"1.1.0-beta.1", Stable, 1, false, ErrVerification},
		{"1.1.0", Beta, 1, true, ErrVerification},
		{"1.1.0", Stable, 2, false, ErrConflict},
	} {
		t.Run(tc.version+string(tc.channel), func(t *testing.T) {
			f := newFixture(t)
			f.github.add(t, tc.version, tc.schema)
			f.github.release(tc.version).Draft = tc.draft
			if _, err := f.m.Stage(context.Background(), tc.version, tc.channel); !errors.Is(err, tc.kind) {
				t.Fatalf("got %v, want %v", err, tc.kind)
			}
			if f.service.current() != "1.0.0" || len(f.service.calls) > 0 {
				t.Fatal("rejected release changed live panel")
			}
		})
	}
	f := newFixture(t)
	f.github.add(t, "1.1.0-beta.1", 1)
	_, err := f.m.Stage(context.Background(), "1.1.0-beta.1", Beta)
	must(t, err)
}

func TestOfficialAssetIdentityAndUpstreamErrors(t *testing.T) {
	for _, address := range []string{"http://127.0.0.1/private", "https://example.com/panel", "https://github.com/attacker/fork/releases/download/v1.1.0/release.json", "https://github.com:444/" + Repository + "/release.json"} {
		t.Run(address, func(t *testing.T) {
			f := newFixture(t)
			f.github.add(t, "1.1.0", 1)
			for i := range f.github.release("1.1.0").Assets {
				if f.github.release("1.1.0").Assets[i].Name == "release.json" {
					f.github.release("1.1.0").Assets[i].URL = address
				}
			}
			if _, err := f.m.Check(context.Background(), Stable); !errors.Is(err, ErrVerification) {
				t.Fatal("nonofficial asset accepted", err)
			}
		})
	}
	f := newFixture(t)
	f.github.hook = func(r *http.Request) (*http.Response, error) { return response(r, 403, nil), nil }
	if _, err := f.m.Check(context.Background(), Stable); !errors.Is(err, ErrUpstream) || HTTPStatus(err) != 502 {
		t.Fatal(err)
	}
}

func TestGitHubEncodedBuildMetadataAndInvalidListings(t *testing.T) {
	v := "1.1.0+build.123"
	name := archiveName(v, "amd64")
	address := strings.ReplaceAll(downloadURL(v, name), "+", "%2B")
	r := githubRelease{Tag: "v" + v, Assets: []githubAsset{{name, 123, address}}}
	if _, err := releaseAsset(r, name); err != nil {
		t.Fatal("GitHub's escaped plus in a valid SemVer download was rejected", err)
	}
	for _, body := range []string{"null", " \nnull\n", "{}", "not JSON"} {
		t.Run(body, func(t *testing.T) {
			f := newFixture(t)
			f.github.hook = func(r *http.Request) (*http.Response, error) { return response(r, 200, []byte(body)), nil }
			if _, err := f.m.Check(context.Background(), Stable); !errors.Is(err, ErrUpstream) {
				t.Fatal("invalid release listing reported no updates", err)
			}
		})
	}
}

func TestRedirectRestrictionsRemainWithInjectedTransport(t *testing.T) {
	for _, address := range []string{"http://127.0.0.1/secret", "https://127.0.0.1/secret", "https://attacker.example/secret"} {
		t.Run(address, func(t *testing.T) {
			f := newFixture(t)
			f.github.hook = func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.github.com" {
					t.Fatalf("unsafe redirect followed: %s", r.URL)
				}
				resp := response(r, 302, nil)
				resp.Header.Set("Location", address)
				return resp, nil
			}
			if _, err := f.m.Check(context.Background(), Stable); !errors.Is(err, ErrUpstream) {
				t.Fatal(err)
			}
		})
	}
}

type cancelBody struct {
	io.Reader
	cancel context.CancelFunc
}

func (b cancelBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.cancel()
	return n, err
}

func TestCancellationBeforeAndDuringDownload(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.m.Check(ctx, Stable); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := f.m.Stage(ctx, "1.1.0", Stable); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.github.requests != 0 {
		t.Fatal("canceled request reached transport")
	}
	f.github.add(t, "1.1.0", 1)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	f.github.hook = func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, ".tar.gz") {
			b := f.github.bodies[r.URL.String()]
			resp := response(r, 200, b)
			resp.Body = io.NopCloser(cancelBody{bytes.NewReader(b), cancel})
			return resp, nil
		}
		return nil, nil
	}
	if _, err := f.m.Stage(ctx, "1.1.0", Stable); !errors.Is(err, context.Canceled) {
		t.Fatal("download cancellation lost", err)
	}
	if j := state(t, f.m); j.Transaction.State != Failed || len(f.service.calls) != 0 {
		t.Fatal("canceled stage touched running panel", j)
	}
}
