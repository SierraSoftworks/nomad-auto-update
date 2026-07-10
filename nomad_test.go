package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestJobSubmissionReadsCurrentVersion guards against regressing to reading the
// submission for version 0: the job list endpoint does not report a version, so
// the current version must come from the job endpoint.
func TestJobSubmissionReadsCurrentVersion(t *testing.T) {
	var requestedVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/job/grey":
			_, _ = w.Write([]byte(`{"ID":"grey","Version":5}`))
		case "/v1/job/grey/submission":
			requestedVersion = r.URL.Query().Get("version")
			_, _ = w.Write([]byte(`{"Format":"hcl2","Source":"job \"grey\" {}","VariableFlags":{"grey_version":"v5"}}`))
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newNomadClient(srv.URL, "", "default", "autoupdate")
	sub, version, err := c.jobSubmission(context.Background(), "default", "grey")
	if err != nil {
		t.Fatal(err)
	}
	if version != 5 {
		t.Fatalf("version = %d, want 5", version)
	}
	if requestedVersion != "5" {
		t.Fatalf("submission requested version=%q, want 5 (must not default to 0)", requestedVersion)
	}
	if sub == nil || sub.VariableFlags["grey_version"] != "v5" {
		t.Fatalf("submission = %+v", sub)
	}
}

// TestRequiredCapabilityHint pins each request in the update flow to the
// specific ACL capability a 403 implicates, so the error points at the missing
// grant rather than the whole policy.
func TestRequiredCapabilityHint(t *testing.T) {
	cases := []struct {
		method, path string
		want         string
	}{
		{http.MethodGet, "/v1/jobs", "list-jobs"},
		{http.MethodGet, "/v1/job/grey", "read-job"},
		{http.MethodGet, "/v1/job/grey/submission", "read-job"},
		{http.MethodGet, "/v1/job/grey/deployment", "read-job"},
		{http.MethodPost, "/v1/jobs/parse", "parse-job"},
		{http.MethodPost, "/v1/job/grey", "submit-job"},
	}
	for _, tc := range cases {
		got := requiredCapabilityHint(tc.method, tc.path)
		if !strings.Contains(got, tc.want) {
			t.Errorf("requiredCapabilityHint(%q, %q) = %q, want it to name %q", tc.method, tc.path, got, tc.want)
		}
	}
}
