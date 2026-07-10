package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	humane "github.com/sierrasoftworks/humane-errors-go"
)

// reservedInterval is the meta sub-key (under the configured prefix) that sets
// a job's per-job check interval rather than declaring an auto-updated
// variable. A job input variable literally named "interval" therefore cannot
// be auto-updated; this is documented in the README.
const reservedInterval = "interval"

// managedVar is a single job input variable placed under auto-update control
// by a meta entry of the form "<prefix>.<Name> = <Spec>".
type managedVar struct {
	Name string // the HCL2 input variable this binds to
	Spec string // update source spec, e.g. "github:owner/repo?prefix=v2."
}

// managedJob is a Nomad job that carries one or more auto-update variable
// declarations in its meta block.
type managedJob struct {
	Namespace string
	ID        string
	Version   int           // the current job version, used to read its submission
	Interval  time.Duration // 0 means "use the service default"
	Vars      []managedVar
}

// key uniquely identifies a job across namespaces.
func (j managedJob) key() string { return j.Namespace + "/" + j.ID }

// parseManagedJob extracts the auto-update configuration from a job's meta
// block. It returns ok=false when the job declares no auto-updated variables,
// so callers can ignore jobs that opt out simply by not carrying the meta.
func parseManagedJob(namespace, id string, version int, meta map[string]string, prefix string) (managedJob, bool, humane.Error) {
	p := prefix + "."

	specs := map[string]string{}
	job := managedJob{Namespace: namespace, ID: id, Version: version}

	for k, v := range meta {
		rest, ok := strings.CutPrefix(k, p)
		if !ok {
			continue
		}
		switch {
		case rest == reservedInterval:
			d, err := time.ParseDuration(strings.TrimSpace(v))
			if err != nil {
				return managedJob{}, false, humane.Wrap(err,
					fmt.Sprintf("job %s/%s has an invalid %s%s value %q", namespace, id, p, reservedInterval, v),
					"Use a Go duration such as 1h, 30m, or 24h.",
				)
			}
			job.Interval = d
		default:
			specs[rest] = v
		}
	}

	if len(specs) == 0 {
		return managedJob{}, false, nil
	}

	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		job.Vars = append(job.Vars, managedVar{
			Name: name,
			Spec: specs[name],
		})
	}

	return job, true, nil
}

// buildVarFile renders a set of HCL2 input variables as var-file content
// suitable for the Variables field of the Nomad jobs/parse API. Keys are
// emitted in sorted order for deterministic output.
func buildVarFile(vars map[string]string) string {
	if len(vars) == 0 {
		return ""
	}
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s = %q\n", name, hclEscapeTemplates(vars[name]))
	}
	return b.String()
}

// hclEscapeTemplates neutralises HCL2 template markers in a value so an
// upstream-controlled string (a release tag, say) cannot inject an expression
// that Nomad would evaluate while parsing the job. A literal "${" is written
// "$${" and "%{" is written "%%{"; the surrounding %q handles quote and
// backslash escaping.
func hclEscapeTemplates(s string) string {
	s = strings.ReplaceAll(s, "${", "$${")
	s = strings.ReplaceAll(s, "%{", "%%{")
	return s
}
