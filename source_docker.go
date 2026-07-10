package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	humane "github.com/sierrasoftworks/humane-errors-go"
)

func init() {
	registerSource("docker", newDockerSource)
}

// dockerManifestAccept lists the manifest media types the resolver understands.
// Registries return the digest of whichever representation matches, so listing
// both single-image and multi-arch index types yields the digest a job's
// image reference would pull.
var dockerManifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// dockerSource resolves a Docker/OCI image tag to its current digest and
// exposes it as "<tag>@sha256:<digest>", a reference that pins the exact image
// while remaining a drop-in replacement in an image = "name:${var}" field.
type dockerSource struct {
	registry string // registry host, e.g. "ghcr.io"
	repo     string // repository path, e.g. "sierrasoftworks/analytics"
	tag      string // tag being tracked, e.g. "latest"
	http     *http.Client
}

func newDockerSource(locator string, _ url.Values) (Source, humane.Error) {
	if locator == "" {
		return nil, humane.New("docker source is missing an image reference",
			"Use docker:REGISTRY/IMAGE:TAG, e.g. docker:ghcr.io/org/app:latest.")
	}
	registry, repo, tag := parseDockerRef(locator)
	return &dockerSource{registry: registry, repo: repo, tag: tag, http: sourceHTTPClient}, nil
}

func (s *dockerSource) scheme() string { return "docker" }

// parseDockerRef splits an image reference into registry, repository, and tag,
// applying Docker Hub's implicit defaults (registry-1.docker.io and the
// "library/" namespace for single-segment names).
func parseDockerRef(ref string) (registry, repo, tag string) {
	// Ignore any pre-existing digest; we resolve a fresh one from the tag.
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		ref = ref[:at]
	}

	tag = "latest"
	// A ':' after the last '/' delimits the tag.
	if colon := strings.LastIndexByte(ref, ':'); colon >= 0 && colon > strings.LastIndexByte(ref, '/') {
		tag = ref[colon+1:]
		ref = ref[:colon]
	}

	first, rest, hasSlash := strings.Cut(ref, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first, rest, tag
	}

	// No registry host: Docker Hub. Single-segment names live under library/.
	if !hasSlash {
		return "registry-1.docker.io", "library/" + ref, tag
	}
	return "registry-1.docker.io", ref, tag
}

// Latest resolves the tag to its digest and returns "<tag>@<digest>".
func (s *dockerSource) Latest(ctx context.Context) (string, error) {
	digest, err := s.resolveDigest(ctx)
	if err != nil {
		return "", err
	}
	return s.tag + "@" + digest, nil
}

func (s *dockerSource) resolveDigest(ctx context.Context) (string, error) {
	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", s.registry, s.repo, url.PathEscape(s.tag))

	resp, err := s.manifestRequest(ctx, manifestURL, "")
	if err != nil {
		return "", err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("Www-Authenticate")
		resp.Body.Close()
		token, terr := s.fetchToken(ctx, challenge)
		if terr != nil {
			return "", terr
		}
		resp, err = s.manifestRequest(ctx, manifestURL, token)
		if err != nil {
			return "", err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := fmt.Sprintf("registry manifest request for %s/%s:%s returned %s: %s",
			s.registry, s.repo, s.tag, resp.Status, strings.TrimSpace(string(body)))
		if resp.StatusCode == http.StatusNotFound {
			return "", humane.New(msg,
				"Check the image and tag exist and, for a private registry, that credentials are available (private registry auth is a planned addition).")
		}
		return "", humane.New(msg)
	}

	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", humane.New(fmt.Sprintf("registry did not return a digest for %s/%s:%s", s.registry, s.repo, s.tag),
			"The registry omitted the Docker-Content-Digest header; it may not be a compliant OCI/Docker registry.")
	}
	return digest, nil
}

func (s *dockerSource) manifestRequest(ctx context.Context, manifestURL, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", dockerManifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, humane.Wrap(err, fmt.Sprintf("could not reach the registry %s", s.registry),
			"Check network connectivity and that the registry host is reachable from the job.")
	}
	return resp, nil
}

type dockerToken struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

// fetchToken satisfies a registry Bearer challenge anonymously, following the
// realm/service/scope parameters the registry advertised.
func (s *dockerSource) fetchToken(ctx context.Context, challenge string) (string, error) {
	realm, params := parseBearerChallenge(challenge)
	if realm == "" {
		return "", humane.New(fmt.Sprintf("registry %s requires authentication but advertised no token endpoint", s.registry),
			"Anonymous pulls are unavailable here; private registry authentication is a planned addition.")
	}

	tokenURL, err := url.Parse(realm)
	if err != nil {
		return "", humane.Wrap(err, fmt.Sprintf("registry %s advertised an invalid token endpoint %q", s.registry, realm))
	}
	q := tokenURL.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	if scope := params["scope"]; scope != "" {
		q.Set("scope", scope)
	}
	tokenURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", humane.Wrap(err, fmt.Sprintf("could not reach the registry token endpoint for %s", s.registry))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", humane.New(fmt.Sprintf("registry token request for %s returned %s: %s",
			s.registry, resp.Status, strings.TrimSpace(string(body))),
			"For a private image this is expected; private registry authentication is a planned addition.")
	}

	var tok dockerToken
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", humane.Wrap(err, fmt.Sprintf("could not parse the token response from %s", s.registry))
	}
	if tok.Token != "" {
		return tok.Token, nil
	}
	if tok.AccessToken != "" {
		return tok.AccessToken, nil
	}
	return "", humane.New(fmt.Sprintf("registry token response from %s contained no token", s.registry))
}

// parseBearerChallenge extracts the realm and the remaining key="value"
// parameters from a WWW-Authenticate: Bearer header.
func parseBearerChallenge(header string) (realm string, params map[string]string) {
	params = map[string]string{}
	rest, ok := cutBearerScheme(header)
	if !ok {
		return "", params
	}
	for _, part := range splitChallengeParams(rest) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if key == "realm" {
			realm = value
		} else {
			params[key] = value
		}
	}
	return realm, params
}

func cutBearerScheme(header string) (string, bool) {
	h := strings.TrimSpace(header)
	if len(h) < len("Bearer ") || !strings.EqualFold(h[:len("Bearer")], "Bearer") {
		return "", false
	}
	return strings.TrimSpace(h[len("Bearer"):]), true
}

// splitChallengeParams splits comma-separated challenge parameters while
// keeping commas that appear inside quoted values intact.
func splitChallengeParams(s string) []string {
	var parts []string
	var cur strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			cur.WriteRune(r)
		case r == ',' && !inQuotes:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}
