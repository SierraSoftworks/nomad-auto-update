package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitHubLatestNoPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/releases/latest" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(ghRelease{TagName: "v3.4.0"})
	}))
	defer srv.Close()

	s := &githubSource{owner: "o", repo: "r", apiURL: srv.URL, http: srv.Client()}
	got, err := s.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "v3.4.0" {
		t.Fatalf("Latest = %q, want v3.4.0", got)
	}
}

func TestGitHubLatestWithPrefixPicksHighest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/releases" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode([]ghRelease{
			{TagName: "v3.0.0"},
			{TagName: "v2.9.0"},
			{TagName: "v2.10.0"},
			{TagName: "v2.11.0", Prerelease: true},
			{TagName: "v2.8.0", Draft: true},
		})
	}))
	defer srv.Close()

	s := &githubSource{owner: "o", repo: "r", prefix: "v2.", apiURL: srv.URL, http: srv.Client()}
	got, err := s.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "v2.10.0" {
		t.Fatalf("Latest = %q, want v2.10.0 (highest non-prerelease v2)", got)
	}
}

func TestGitHubSendsToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(ghRelease{TagName: "v1.0.0"})
	}))
	defer srv.Close()

	s := &githubSource{owner: "o", repo: "r", token: "secret", apiURL: srv.URL, http: srv.Client()}
	if _, err := s.Latest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization = %q, want Bearer secret", gotAuth)
	}
}

func TestGitHubNoMatchingReleaseErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]ghRelease{{TagName: "v1.0.0"}})
	}))
	defer srv.Close()

	s := &githubSource{owner: "o", repo: "r", prefix: "v2.", apiURL: srv.URL, http: srv.Client()}
	if _, err := s.Latest(context.Background()); err == nil {
		t.Fatal("expected error when no release matches the prefix")
	}
}
