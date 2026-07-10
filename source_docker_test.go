package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDockerLatestResolvesDigestViaToken(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/library/test/manifests/latest":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg",scope="repository:library/test:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "manifest") {
				t.Errorf("Accept header missing manifest types: %q", r.Header.Get("Accept"))
			}
			w.Header().Set("Docker-Content-Digest", "sha256:deadbeef")
			w.WriteHeader(http.StatusOK)
		case "/token":
			if got := r.URL.Query().Get("service"); got != "reg" {
				t.Errorf("token service = %q", got)
			}
			if got := r.URL.Query().Get("scope"); got != "repository:library/test:pull" {
				t.Errorf("token scope = %q", got)
			}
			_ = json.NewEncoder(w).Encode(dockerToken{Token: "tok"})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s := &dockerSource{
		registry: strings.TrimPrefix(srv.URL, "https://"),
		repo:     "library/test",
		tag:      "latest",
		http:     srv.Client(),
	}
	got, err := s.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "latest@sha256:deadbeef" {
		t.Fatalf("Latest = %q, want latest@sha256:deadbeef", got)
	}
}

func TestDockerLatestAnonymousNoDigest(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Respond 200 but omit the digest header.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &dockerSource{
		registry: strings.TrimPrefix(srv.URL, "https://"),
		repo:     "library/test",
		tag:      "latest",
		http:     srv.Client(),
	}
	if _, err := s.Latest(context.Background()); err == nil {
		t.Fatal("expected error when registry omits the digest header")
	}
}
