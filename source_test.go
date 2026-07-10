package main

import "testing"

func TestParseSource(t *testing.T) {
	cases := []struct {
		spec       string
		wantScheme string
		wantErr    bool
	}{
		{spec: "github:SierraSoftworks/grey", wantScheme: "github"},
		{spec: "github:SierraSoftworks/grey?prefix=v2.", wantScheme: "github"},
		{spec: "docker:ghcr.io/sierrasoftworks/analytics:latest", wantScheme: "docker"},
		{spec: "docker:nginx", wantScheme: "docker"},
		{spec: "", wantErr: true},
		{spec: "github", wantErr: true},
		{spec: "ftp:example.com/file", wantErr: true},
		{spec: "github:no-slash", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			src, err := parseSource(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if src.scheme() != tc.wantScheme {
				t.Fatalf("scheme = %q, want %q", src.scheme(), tc.wantScheme)
			}
		})
	}
}

func TestGitHubSourceOptions(t *testing.T) {
	src, err := parseSource("github:SierraSoftworks/grey?prefix=v2.&prerelease=true")
	if err != nil {
		t.Fatal(err)
	}
	gh := src.(*githubSource)
	if gh.owner != "SierraSoftworks" || gh.repo != "grey" {
		t.Fatalf("owner/repo = %s/%s", gh.owner, gh.repo)
	}
	if gh.prefix != "v2." || !gh.prereleases {
		t.Fatalf("prefix=%q prereleases=%v", gh.prefix, gh.prereleases)
	}
}

func TestParseDockerRef(t *testing.T) {
	cases := []struct {
		ref          string
		wantRegistry string
		wantRepo     string
		wantTag      string
	}{
		{"ghcr.io/sierrasoftworks/analytics:latest", "ghcr.io", "sierrasoftworks/analytics", "latest"},
		{"ghcr.io/sierrasoftworks/analytics", "ghcr.io", "sierrasoftworks/analytics", "latest"},
		{"nginx", "registry-1.docker.io", "library/nginx", "latest"},
		{"nginx:1.25", "registry-1.docker.io", "library/nginx", "1.25"},
		{"bitnami/redis:7", "registry-1.docker.io", "bitnami/redis", "7"},
		{"localhost:5000/img:v1", "localhost:5000", "img", "v1"},
		{"localhost:5000/img", "localhost:5000", "img", "latest"},
		{"ghcr.io/o/i@sha256:deadbeef", "ghcr.io", "o/i", "latest"},
	}

	for _, tc := range cases {
		t.Run(tc.ref, func(t *testing.T) {
			reg, repo, tag := parseDockerRef(tc.ref)
			if reg != tc.wantRegistry || repo != tc.wantRepo || tag != tc.wantTag {
				t.Fatalf("parseDockerRef(%q) = %q, %q, %q; want %q, %q, %q",
					tc.ref, reg, repo, tag, tc.wantRegistry, tc.wantRepo, tc.wantTag)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v2.10.0", "v2.9.0", 1},
		{"v2.1.0", "v2.0.5", 1},
		{"v1.0.0", "v2.0.0", -1},
		{"v2.1.0", "v2.1.0", 0},
		{"v2.1.0", "v2.1", 1},
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); sign(got) != tc.want {
			t.Fatalf("compareVersions(%q, %q) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

func TestParseBearerChallenge(t *testing.T) {
	realm, params := parseBearerChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:o/i:pull"`)
	if realm != "https://ghcr.io/token" {
		t.Fatalf("realm = %q", realm)
	}
	if params["service"] != "ghcr.io" {
		t.Fatalf("service = %q", params["service"])
	}
	if params["scope"] != "repository:o/i:pull" {
		t.Fatalf("scope = %q", params["scope"])
	}

	if r, _ := parseBearerChallenge("Basic realm=x"); r != "" {
		t.Fatalf("non-bearer challenge should yield empty realm, got %q", r)
	}
}
