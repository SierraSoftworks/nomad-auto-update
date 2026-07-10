package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	humane "github.com/sierrasoftworks/humane-errors-go"
)

func init() {
	registerSource("github", newGitHubSource)
}

// githubSource resolves the latest release of a GitHub repository, exposing the
// release's tag name (e.g. "v2.1.3"). When a prefix is supplied only tags with
// that prefix are considered, which pins a job to a major/minor line while
// still tracking its newest patch release.
type githubSource struct {
	owner       string
	repo        string
	prefix      string // only tags with this prefix qualify ("" = any)
	prereleases bool   // include pre-releases
	apiURL      string
	token       string
	http        *http.Client
}

func newGitHubSource(locator string, opts url.Values) (Source, humane.Error) {
	owner, repo, ok := strings.Cut(locator, "/")
	if !ok || owner == "" || repo == "" {
		return nil, humane.New(fmt.Sprintf("github source %q is not in OWNER/REPO form", locator),
			"Use github:OWNER/REPO, optionally with ?prefix=v2. to constrain the release line.")
	}
	return &githubSource{
		owner:       owner,
		repo:        strings.TrimSuffix(repo, ".git"),
		prefix:      opts.Get("prefix"),
		prereleases: opts.Get("prerelease") == "true",
		apiURL:      strings.TrimRight(envOr("GITHUB_API_URL", "https://api.github.com"), "/"),
		token:       envOr("GITHUB_TOKEN", ""),
		http:        sourceHTTPClient,
	}, nil
}

func (s *githubSource) scheme() string { return "github" }

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Latest returns the tag of the newest qualifying release. With no prefix and
// pre-releases excluded it uses the repository's "latest release" endpoint;
// otherwise it lists releases and selects the highest-versioned match.
func (s *githubSource) Latest(ctx context.Context) (string, error) {
	if s.prefix == "" && !s.prereleases {
		var rel ghRelease
		if err := s.get(ctx, fmt.Sprintf("/repos/%s/%s/releases/latest", s.owner, s.repo), &rel); err != nil {
			return "", err
		}
		if rel.TagName == "" {
			return "", humane.New(fmt.Sprintf("github repository %s/%s has no published releases", s.owner, s.repo),
				"Publish a release, or point the source at a repository that has one.")
		}
		return rel.TagName, nil
	}

	var releases []ghRelease
	if err := s.get(ctx, fmt.Sprintf("/repos/%s/%s/releases?per_page=100", s.owner, s.repo), &releases); err != nil {
		return "", err
	}

	best := ""
	for _, rel := range releases {
		if rel.Draft || (rel.Prerelease && !s.prereleases) {
			continue
		}
		if s.prefix != "" && !strings.HasPrefix(rel.TagName, s.prefix) {
			continue
		}
		if best == "" || compareVersions(rel.TagName, best) > 0 {
			best = rel.TagName
		}
	}
	if best == "" {
		return "", humane.New(fmt.Sprintf("github repository %s/%s has no release matching prefix %q", s.owner, s.repo, s.prefix),
			"Check the prefix matches published tag names, and that the releases are not all drafts or pre-releases.")
	}
	return best, nil
}

func (s *githubSource) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return humane.Wrap(err, fmt.Sprintf("could not reach the GitHub API for %s/%s", s.owner, s.repo),
			"Check network connectivity and that api.github.com (or $GITHUB_API_URL) is reachable from the job.")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := fmt.Sprintf("GitHub API GET %s returned %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
		switch resp.StatusCode {
		case http.StatusNotFound:
			return humane.New(msg,
				fmt.Sprintf("Check the repository %s/%s exists and, if private, that GITHUB_TOKEN grants access.", s.owner, s.repo))
		case http.StatusForbidden, http.StatusTooManyRequests:
			return humane.New(msg,
				"This is usually GitHub API rate limiting; set GITHUB_TOKEN to raise the limit.")
		default:
			return humane.New(msg)
		}
	}

	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return humane.Wrap(err, fmt.Sprintf("could not parse the GitHub API response for %s", path))
	}
	return nil
}

// compareVersions orders two version-like strings by their numeric segments,
// so v2.10.0 sorts above v2.9.0. Non-numeric characters delimit segments; a
// purely lexical comparison breaks the tie when the numeric parts are equal.
func compareVersions(a, b string) int {
	as, bs := versionSegments(a), versionSegments(b)
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] != bs[i] {
			if as[i] < bs[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return strings.Compare(a, b)
}

func versionSegments(s string) []int {
	var segs []int
	cur := ""
	flush := func() {
		if cur != "" {
			n, _ := strconv.Atoi(cur)
			segs = append(segs, n)
			cur = ""
		}
	}
	for _, r := range s {
		if r >= '0' && r <= '9' {
			cur += string(r)
		} else {
			flush()
		}
	}
	flush()
	return segs
}
