package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	humane "github.com/sierrasoftworks/humane-errors-go"
)

const greySource = `job "grey" {
  group "app" {}
}`

// fakeNomad is an in-memory nomadAPI for exercising the updater without a real
// Nomad agent.
type fakeNomad struct {
	jobs        []managedJob
	listErr     error
	submissions map[string]*submission
	active      map[string]bool
	parsed      json.RawMessage
	parseErr    error

	parseVarFiles []string
	parseSources  []string
	registered    []registeredJob
}

type registeredJob struct {
	namespace string
	id        string
	job       json.RawMessage
	sub       *submission
}

func (f *fakeNomad) listAutoUpdateJobs(context.Context) ([]managedJob, error) {
	return f.jobs, f.listErr
}

func (f *fakeNomad) jobSubmission(_ context.Context, ns, id string, _ int) (*submission, error) {
	return f.submissions[ns+"/"+id], nil
}

func (f *fakeNomad) deploymentActive(_ context.Context, ns, id string) (bool, error) {
	return f.active[ns+"/"+id], nil
}

func (f *fakeNomad) parseJob(_ context.Context, _, hcl, varFile string) (json.RawMessage, error) {
	f.parseSources = append(f.parseSources, hcl)
	f.parseVarFiles = append(f.parseVarFiles, varFile)
	if f.parseErr != nil {
		return nil, f.parseErr
	}
	return f.parsed, nil
}

func (f *fakeNomad) registerJob(_ context.Context, ns, id string, job json.RawMessage, sub *submission) error {
	f.registered = append(f.registered, registeredJob{namespace: ns, id: id, job: job, sub: sub})
	return nil
}

// staticSources resolves each spec to a preset value.
func staticSources(values map[string]string) func(spec string) (Source, humane.Error) {
	return func(spec string) (Source, humane.Error) {
		v, ok := values[spec]
		if !ok {
			return nil, humane.New("no fake value for spec " + spec)
		}
		return &staticSource{value: v}, nil
	}
}

type staticSource struct{ value string }

func (s *staticSource) Latest(context.Context) (string, error) { return s.value, nil }
func (s *staticSource) scheme() string                         { return "fake" }

func greyJob(current string) *fakeNomad {
	return &fakeNomad{
		submissions: map[string]*submission{
			"default/grey": {
				Format:        "hcl2",
				Source:        greySource,
				VariableFlags: map[string]string{"grey_version": current},
			},
		},
		active: map[string]bool{},
		parsed: json.RawMessage(`{"ID":"grey"}`),
	}
}

func greyManagedJob() managedJob {
	return managedJob{
		Namespace: "default",
		ID:        "grey",
		Version:   2,
		Vars:      []managedVar{{Name: "grey_version", Spec: "github:o/grey"}},
	}
}

func greyUpdater(nomad nomadAPI) *updater {
	return &updater{nomad: nomad, newSource: staticSources(map[string]string{"github:o/grey": "v2.2.0"})}
}

func TestUpdaterAppliesChange(t *testing.T) {
	nomad := greyJob("v2.1.0")
	u := greyUpdater(nomad)

	updated, err := u.check(context.Background(), greyManagedJob())
	if err != nil {
		t.Fatal(err)
	}
	if !updated {
		t.Fatal("expected an update to be applied")
	}
	if len(nomad.registered) != 1 {
		t.Fatalf("expected 1 registration, got %d", len(nomad.registered))
	}

	// The parse step re-renders the original HCL with the new variable value.
	if len(nomad.parseVarFiles) != 1 || !strings.Contains(nomad.parseVarFiles[0], `grey_version = "v2.2.0"`) {
		t.Fatalf("parse var-file = %q", nomad.parseVarFiles)
	}
	// The job's own HCL is left untouched; only the variables change.
	if nomad.parseSources[0] != greySource {
		t.Fatalf("parse source was modified:\n%s", nomad.parseSources[0])
	}

	reg := nomad.registered[0]
	if reg.sub == nil || reg.sub.Format != "hcl2" || reg.sub.Source != greySource {
		t.Fatalf("submission source changed: %+v", reg.sub)
	}
	if reg.sub.VariableFlags["grey_version"] != "v2.2.0" {
		t.Fatalf("submission var flags = %v", reg.sub.VariableFlags)
	}
}

func TestUpdaterNoChange(t *testing.T) {
	nomad := greyJob("v2.2.0")
	u := greyUpdater(nomad)

	updated, err := u.check(context.Background(), greyManagedJob())
	if err != nil {
		t.Fatal(err)
	}
	if updated || len(nomad.registered) != 0 {
		t.Fatalf("expected no update; updated=%v registrations=%d", updated, len(nomad.registered))
	}
}

func TestUpdaterUpdatesWhenNoVarFlagsRecorded(t *testing.T) {
	// A job registered without -var flags (e.g. via a var file) has no recorded
	// current value, so the first resolved value is treated as a change.
	nomad := &fakeNomad{
		submissions: map[string]*submission{
			"default/grey": {Format: "hcl2", Source: greySource},
		},
		active: map[string]bool{},
		parsed: json.RawMessage(`{"ID":"grey"}`),
	}
	u := greyUpdater(nomad)

	updated, err := u.check(context.Background(), greyManagedJob())
	if err != nil {
		t.Fatal(err)
	}
	if !updated || len(nomad.registered) != 1 {
		t.Fatalf("expected an update; updated=%v registrations=%d", updated, len(nomad.registered))
	}
	if nomad.registered[0].sub.VariableFlags["grey_version"] != "v2.2.0" {
		t.Fatalf("expected grey_version set, got %v", nomad.registered[0].sub.VariableFlags)
	}
}

func TestUpdaterSkipsWhenSourceNotRetained(t *testing.T) {
	nomad := &fakeNomad{submissions: map[string]*submission{}, active: map[string]bool{}}
	u := greyUpdater(nomad)

	updated, err := u.check(context.Background(), greyManagedJob())
	if err != nil {
		t.Fatalf("skip should not be an error: %v", err)
	}
	if updated || len(nomad.registered) != 0 {
		t.Fatal("expected no registration when source is not retained")
	}
}

func TestUpdaterDefersWhenDeploymentActive(t *testing.T) {
	nomad := greyJob("v2.1.0")
	nomad.active["default/grey"] = true
	u := greyUpdater(nomad)

	updated, err := u.check(context.Background(), greyManagedJob())
	if err != nil {
		t.Fatal(err)
	}
	if updated || len(nomad.registered) != 0 {
		t.Fatal("expected the update to be deferred while a deployment is active")
	}
}

func TestUpdaterDryRunDoesNotRegister(t *testing.T) {
	nomad := greyJob("v2.1.0")
	u := greyUpdater(nomad)
	u.dryRun = true

	updated, err := u.check(context.Background(), greyManagedJob())
	if err != nil {
		t.Fatal(err)
	}
	if updated || len(nomad.registered) != 0 {
		t.Fatal("dry-run must not register a job")
	}
	if len(nomad.parseVarFiles) != 0 {
		t.Fatal("dry-run must not re-render the job")
	}
}

func TestUpdaterReportsSourceErrorWhenNoChange(t *testing.T) {
	nomad := greyJob("v2.1.0")
	u := &updater{nomad: nomad, newSource: func(string) (Source, humane.Error) {
		return &errSource{}, nil
	}}

	updated, err := u.check(context.Background(), greyManagedJob())
	if err == nil {
		t.Fatal("expected the source error to surface when nothing could be resolved")
	}
	if updated {
		t.Fatal("no update should be reported on source failure")
	}
}

type errSource struct{}

func (errSource) Latest(context.Context) (string, error) { return "", errors.New("boom") }
func (errSource) scheme() string                         { return "fake" }
