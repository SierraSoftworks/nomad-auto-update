package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	humane "github.com/sierrasoftworks/humane-errors-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// nomadClient is a minimal client for the handful of Nomad HTTP API endpoints
// the service needs. It deliberately avoids the official api package so the
// service stays dependency-light.
//
// It supports plain http(s) addresses as well as unix domain sockets
// ("unix:///path/to/api.sock"), the latter being how tasks reach Nomad's task
// API from inside an allocation using their workload identity.
type nomadClient struct {
	http       *http.Client
	base       string
	addr       string // as configured, for error messages
	token      string
	namespace  string // namespace to scan/operate in ("*" scans all authorized)
	metaPrefix string
}

func newNomadClient(addr, token, namespace, metaPrefix string) *nomadClient {
	client := &http.Client{}
	base := strings.TrimRight(addr, "/")

	if sock, ok := strings.CutPrefix(addr, "unix://"); ok {
		client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		}
		base = "http://nomad.task.api"
	}

	return &nomadClient{
		http:       client,
		base:       base,
		addr:       addr,
		token:      token,
		namespace:  namespace,
		metaPrefix: metaPrefix,
	}
}

// do issues a request and returns the raw response without checking its status
// code. It records a client span and request-duration metric per call.
func (c *nomadClient) do(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Response, error) {
	route := nomadRoute(path)
	ctx, span := tracer.Start(ctx, method+" "+route,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", method),
			attribute.String("url.path", path),
			attribute.String("http.route", route),
			attribute.String("server.address", c.addr),
		))
	defer span.End()

	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("X-Nomad-Token", c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	started := time.Now()
	resp, err := c.http.Do(req)
	mNomadRequestDuration.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(
		attribute.String("http.route", route),
		attribute.Bool("error", err != nil),
	))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "request failed")
		return nil, humane.Wrap(err, "could not reach the Nomad API at "+c.addr,
			"Check that a Nomad agent is listening at the configured address: the -nomad-addr flag, $NOMAD_ADDR, or (inside a Nomad task) the api.sock task API socket.",
		)
	}
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	return resp, nil
}

// doJSON issues a request, requires a 200 response, and decodes the body into
// v (which may be nil to discard it).
func (c *nomadClient) doJSON(ctx context.Context, method, path string, query url.Values, body []byte, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return c.statusError(method, path, resp)
	}
	if v == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return humane.Wrap(err, fmt.Sprintf("could not parse the Nomad API response for %s %s", method, path),
			"The configured address may not be a Nomad agent's HTTP API; check the -nomad-addr flag and $NOMAD_ADDR.")
	}
	return nil
}

func (c *nomadClient) statusError(method, path string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := fmt.Sprintf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	if resp.StatusCode == http.StatusForbidden {
		return humane.New(msg,
			requiredCapabilityHint(method, path),
			`Grant it in the workload identity's namespace policy: capabilities = ["list-jobs", "read-job", "parse-job", "submit-job"].`,
			`Apply the policy with: nomad acl policy apply -namespace default -job nomad-auto-update nomad-auto-update policy.hcl — see the README.`,
		)
	}
	return humane.New(msg)
}

// requiredCapabilityHint maps a denied request to the specific Nomad ACL
// capability it needs, so a 403 points straight at the missing grant instead of
// the whole policy. Reads need read-job/list-jobs; the parse (re-render) step
// needs parse-job; registering the new version needs submit-job.
func requiredCapabilityHint(method, path string) string {
	switch {
	case method == http.MethodGet && path == "/v1/jobs":
		return "Discovering jobs (GET /v1/jobs) needs the list-jobs capability on the scanned namespaces."
	case method == http.MethodGet:
		return fmt.Sprintf("Reading a job (%s %s) needs the read-job capability on the job's namespace.", method, path)
	case path == "/v1/jobs/parse":
		return "Re-rendering the HCL (POST /v1/jobs/parse) needs the parse-job capability (or submit-job) on the job's namespace."
	default:
		return fmt.Sprintf("Registering the updated job (%s %s) needs the submit-job capability on the job's namespace; if the job mounts a host volume it also needs a host_volume policy block granting mount-readwrite (CSI volumes need csi-mount-volume plus plugin read).", method, path)
	}
}

// nomadRoute maps a request path to a low-cardinality route template for use
// as a span name and metric attribute.
func nomadRoute(path string) string {
	switch {
	case path == "/v1/jobs":
		return "/v1/jobs"
	case path == "/v1/jobs/parse":
		return "/v1/jobs/parse"
	case strings.HasSuffix(path, "/submission"):
		return "/v1/job/:id/submission"
	case strings.HasSuffix(path, "/deployment"):
		return "/v1/job/:id/deployment"
	case strings.HasPrefix(path, "/v1/job/"):
		return "/v1/job/:id"
	default:
		return path
	}
}

// jobListStub is a subset of GET /v1/jobs entries: enough to identify a job and
// read its meta. The list endpoint does not report a job's version, so the
// current version is read separately (see currentVersion) at check time.
type jobListStub struct {
	ID        string
	Namespace string
	Status    string
	Version   int
	Meta      map[string]string
}

// listAutoUpdateJobs returns every job carrying auto-update meta in the
// configured namespace scope.
func (c *nomadClient) listAutoUpdateJobs(ctx context.Context) ([]managedJob, error) {
	var stubs []jobListStub
	if err := c.doJSON(ctx, http.MethodGet, "/v1/jobs", url.Values{
		"namespace": {c.namespace},
		"meta":      {"true"},
	}, nil, &stubs); err != nil {
		return nil, err
	}

	var jobs []managedJob
	for _, stub := range stubs {
		if stub.Status == "dead" {
			continue
		}
		job, ok, err := parseManagedJob(stub.Namespace, stub.ID, stub.Meta, c.metaPrefix)
		if err != nil {
			log(ctx).With(zap.String("namespace", stub.Namespace), zap.String("job", stub.ID)).
				Warn("ignoring invalid auto-update config", humane.Zap(err)...)
			continue
		}
		if ok {
			jobs = append(jobs, job)
		}
	}
	return jobs, nil
}

// submission is the original source of a job version, as returned by the
// job-submission API and accepted (partially) by the register API.
type submission struct {
	Format        string            `json:"Format,omitempty"`
	Source        string            `json:"Source,omitempty"`
	VariableFlags map[string]string `json:"VariableFlags,omitempty"`
	Variables     string            `json:"Variables,omitempty"`
}

// currentVersion reports the current (latest) version number of a job. The job
// list endpoint does not report it, so it is read from the job endpoint.
func (c *nomadClient) currentVersion(ctx context.Context, namespace, id string) (int, error) {
	var job struct {
		Version int
	}
	path := "/v1/job/" + url.PathEscape(id)
	if err := c.doJSON(ctx, http.MethodGet, path, url.Values{"namespace": {namespace}}, nil, &job); err != nil {
		return 0, err
	}
	return job.Version, nil
}

// jobSubmission reads the original HCL source and variable values for a job's
// current version, also returning that version number. It returns a nil
// submission (no error) when Nomad has no source on file for that version.
func (c *nomadClient) jobSubmission(ctx context.Context, namespace, id string) (*submission, int, error) {
	version, err := c.currentVersion(ctx, namespace, id)
	if err != nil {
		return nil, 0, err
	}
	sub, err := c.submissionForVersion(ctx, namespace, id, version)
	return sub, version, err
}

// submissionForVersion reads the original HCL source and variable values for a
// specific job version. It returns a nil submission (no error) when Nomad has
// no source on file for that version.
func (c *nomadClient) submissionForVersion(ctx context.Context, namespace, id string, version int) (*submission, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	path := "/v1/job/" + url.PathEscape(id) + "/submission"
	resp, err := c.do(ctx, http.MethodGet, path, url.Values{
		"namespace": {namespace},
		"version":   {fmt.Sprintf("%d", version)},
	}, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, c.statusError(http.MethodGet, path, resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	// The endpoint returns a JSON null when no source is retained.
	if len(bytes.TrimSpace(body)) == 0 || string(bytes.TrimSpace(body)) == "null" {
		return nil, nil
	}
	var sub submission
	if err := json.Unmarshal(body, &sub); err != nil {
		return nil, humane.Wrap(err, fmt.Sprintf("could not parse the job submission for %s/%s", namespace, id))
	}
	return &sub, nil
}

// deploymentActive reports whether the job has an in-progress deployment, in
// which case an update should wait for the rollout to settle.
func (c *nomadClient) deploymentActive(ctx context.Context, namespace, id string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	path := "/v1/job/" + url.PathEscape(id) + "/deployment"
	resp, err := c.do(ctx, http.MethodGet, path, url.Values{"namespace": {namespace}}, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, c.statusError(http.MethodGet, path, resp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, err
	}
	if len(bytes.TrimSpace(body)) == 0 || string(bytes.TrimSpace(body)) == "null" {
		return false, nil
	}
	var dep struct {
		Status string
	}
	if err := json.Unmarshal(body, &dep); err != nil {
		return false, humane.Wrap(err, fmt.Sprintf("could not parse the deployment for %s/%s", namespace, id))
	}
	return deploymentIsActive(dep.Status), nil
}

// deploymentIsActive reports whether a deployment status represents an
// in-progress rollout (i.e. not one of the terminal states).
func deploymentIsActive(status string) bool {
	switch status {
	case "", "successful", "failed", "cancelled":
		return false
	default:
		return true
	}
}

type parseRequest struct {
	JobHCL       string
	Variables    string `json:",omitempty"`
	Canonicalize bool
}

// parseJob renders a job's HCL to its JSON representation, applying the given
// var-file content as HCL2 input variables.
func (c *nomadClient) parseJob(ctx context.Context, namespace, hcl, varFile string) (json.RawMessage, error) {
	body, err := json.Marshal(parseRequest{JobHCL: hcl, Variables: varFile, Canonicalize: true})
	if err != nil {
		return nil, err
	}
	var job json.RawMessage
	if err := c.doJSON(ctx, http.MethodPost, "/v1/jobs/parse", url.Values{"namespace": {namespace}}, body, &job); err != nil {
		return nil, humane.Wrap(err, "could not parse the job HCL with the updated variables",
			"The job source must be valid HCL2; a job registered with nomad_discard_job_source=true cannot be re-rendered.")
	}
	return job, nil
}

type registerRequest struct {
	Job        json.RawMessage
	Submission *submission `json:",omitempty"`
}

type registerResponse struct {
	EvalID   string
	Warnings string
}

// registerJob registers a new version of the job, carrying the updated source
// submission so the next cycle can read the new variable values back.
func (c *nomadClient) registerJob(ctx context.Context, namespace, id string, job json.RawMessage, sub *submission) error {
	body, err := json.Marshal(registerRequest{Job: job, Submission: sub})
	if err != nil {
		return err
	}
	var out registerResponse
	path := "/v1/job/" + url.PathEscape(id)
	if err := c.doJSON(ctx, http.MethodPost, path, url.Values{"namespace": {namespace}}, body, &out); err != nil {
		return err
	}
	if strings.TrimSpace(out.Warnings) != "" {
		log(ctx).With(zap.String("namespace", namespace), zap.String("job", id)).
			Warn("Nomad returned warnings on register", zap.String("warnings", strings.TrimSpace(out.Warnings)))
	}
	return nil
}
