package main

import (
	"testing"
	"time"
)

func TestParseManagedJob(t *testing.T) {
	prefix := "autoupdate"

	cases := []struct {
		name         string
		meta         map[string]string
		wantOK       bool
		wantErr      bool
		wantInterval time.Duration
		wantVars     map[string]managedVar
	}{
		{
			name:   "no autoupdate meta opts out",
			meta:   map[string]string{"team": "platform"},
			wantOK: false,
		},
		{
			name: "declares github and docker variables",
			meta: map[string]string{
				"autoupdate.grey_version":      "github:SierraSoftworks/grey?prefix=v2.",
				"autoupdate.analytics_version": "docker:ghcr.io/sierrasoftworks/analytics:latest",
				"team":                         "platform",
			},
			wantOK: true,
			wantVars: map[string]managedVar{
				"grey_version":      {Name: "grey_version", Spec: "github:SierraSoftworks/grey?prefix=v2."},
				"analytics_version": {Name: "analytics_version", Spec: "docker:ghcr.io/sierrasoftworks/analytics:latest"},
			},
		},
		{
			name: "per-job interval is parsed and reserved",
			meta: map[string]string{
				"autoupdate.interval":     "30m",
				"autoupdate.grey_version": "github:o/r",
			},
			wantOK:       true,
			wantInterval: 30 * time.Minute,
			wantVars: map[string]managedVar{
				"grey_version": {Name: "grey_version", Spec: "github:o/r"},
			},
		},
		{
			name:    "invalid interval is an error",
			meta:    map[string]string{"autoupdate.interval": "soon", "autoupdate.x": "github:o/r"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job, ok, err := parseManagedJob("default", "example", 3, tc.meta, prefix)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if job.Namespace != "default" || job.ID != "example" || job.Version != 3 {
				t.Fatalf("identity = %s/%s v%d, want default/example v3", job.Namespace, job.ID, job.Version)
			}
			if job.Interval != tc.wantInterval {
				t.Fatalf("interval = %s, want %s", job.Interval, tc.wantInterval)
			}
			if len(job.Vars) != len(tc.wantVars) {
				t.Fatalf("got %d vars, want %d", len(job.Vars), len(tc.wantVars))
			}
			for _, got := range job.Vars {
				want, exists := tc.wantVars[got.Name]
				if !exists {
					t.Fatalf("unexpected var %q", got.Name)
				}
				if got != want {
					t.Fatalf("var %q = %+v, want %+v", got.Name, got, want)
				}
			}
		})
	}
}

func TestParseManagedJobVarsSorted(t *testing.T) {
	job, ok, err := parseManagedJob("default", "example", 0, map[string]string{
		"autoupdate.zebra": "github:o/z",
		"autoupdate.alpha": "github:o/a",
	}, "autoupdate")
	if err != nil || !ok {
		t.Fatalf("parse failed: ok=%v err=%v", ok, err)
	}
	if job.Vars[0].Name != "alpha" || job.Vars[1].Name != "zebra" {
		t.Fatalf("vars not sorted: %s, %s", job.Vars[0].Name, job.Vars[1].Name)
	}
}

func TestBuildVarFile(t *testing.T) {
	got := buildVarFile(map[string]string{
		"grey_version":      "v2.2.0",
		"analytics_version": "latest@sha256:abc",
	})
	want := "analytics_version = \"latest@sha256:abc\"\ngrey_version = \"v2.2.0\"\n"
	if got != want {
		t.Fatalf("buildVarFile = %q, want %q", got, want)
	}
	if buildVarFile(nil) != "" {
		t.Fatalf("empty var file should be empty string")
	}
}

func TestBuildVarFileNeutralisesTemplateMarkers(t *testing.T) {
	// An upstream-controlled value must not smuggle an HCL template expression
	// into the var-file that Nomad would evaluate at parse time.
	got := buildVarFile(map[string]string{"x": "v1${file(\"/etc/passwd\")}%{if true}"})
	want := "x = \"v1$${file(\\\"/etc/passwd\\\")}%%{if true}\"\n"
	if got != want {
		t.Fatalf("buildVarFile = %q, want %q", got, want)
	}
}
