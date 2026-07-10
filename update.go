package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	humane "github.com/sierrasoftworks/humane-errors-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// nomadAPI is the subset of Nomad operations the updater depends on, extracted
// so tests can drive it with an in-memory implementation.
type nomadAPI interface {
	listAutoUpdateJobs(ctx context.Context) ([]managedJob, error)
	jobSubmission(ctx context.Context, namespace, id string, version int) (*submission, error)
	deploymentActive(ctx context.Context, namespace, id string) (bool, error)
	parseJob(ctx context.Context, namespace, hcl, varFile string) (json.RawMessage, error)
	registerJob(ctx context.Context, namespace, id string, job json.RawMessage, sub *submission) error
}

// updater performs a single check-and-update pass for one job: read its
// submission, resolve each declared source, and — when a value changed —
// re-render the job's HCL with the new variables and register a new version.
//
// It consults a versionCache of the values it has applied per job variable. If
// Nomad auto-reverts a failed deployment, the job's variable returns to its old
// value while the source still reports the newer version; the cache lets the
// updater recognise a version it already applied and avoid pushing it again in
// a loop. The cache is persisted (see versionCache), so this holds across
// restarts.
type updater struct {
	nomad     nomadAPI
	newSource func(spec string) (Source, humane.Error)
	dryRun    bool
	cache     *versionCache
}

func newUpdater(nomad nomadAPI, dryRun bool, cache *versionCache) *updater {
	return &updater{
		nomad:     nomad,
		newSource: parseSource,
		dryRun:    dryRun,
		cache:     cache,
	}
}

// check runs one update pass for a job. It returns true when a new job version
// was registered. A nil error with false means "nothing to do" (up to date,
// skipped, or dry-run).
func (u *updater) check(ctx context.Context, job managedJob) (updated bool, err error) {
	ctx, span := tracer.Start(ctx, "check", trace.WithAttributes(
		attribute.String("nomad.namespace", job.Namespace),
		attribute.String("nomad.job.id", job.ID),
		attribute.Int("nomad.job.version", job.Version),
	))
	defer span.End()

	started := time.Now()
	outcome := "no_change"
	defer func() {
		span.SetAttributes(attribute.String("autoupdate.outcome", outcome))
		mChecks.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
		mCheckDuration.Record(ctx, time.Since(started).Seconds())
	}()

	sub, subErr := u.nomad.jobSubmission(ctx, job.Namespace, job.ID, job.Version)
	if subErr != nil {
		outcome = "error"
		span.RecordError(subErr)
		span.SetStatus(codes.Error, "submission read failed")
		return false, subErr
	}
	if sub == nil || sub.Format != "hcl2" || strings.TrimSpace(sub.Source) == "" {
		outcome = "skipped"
		mChecksSkipped.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "no_source")))
		log(ctx).Warn("cannot auto-update job: source not retained", humane.Zap(humane.New(
			fmt.Sprintf("no HCL source is retained for %s/%s version %d", job.Namespace, job.ID, job.Version),
			"Auto-update re-renders the job's HCL, so the source must be retained; do not set meta.nomad_discard_job_source = true on the job.",
			"Re-register the job once with its source retained (the default) so its variables can be updated.",
		))...)
		return false, nil
	}

	// Current values come from the submission's -var flags.
	base := map[string]string{}
	for k, v := range sub.VariableFlags {
		base[k] = v
	}

	changes := map[string]string{} // variable -> latest (differs from current)
	var resolveErrs []error

	for _, mv := range job.Vars {
		latest, rErr := u.resolve(ctx, mv)
		if rErr != nil {
			resolveErrs = append(resolveErrs, rErr)
			log(ctx).With(zap.String("spec", mv.Spec), zap.String("namespace", job.Namespace), zap.String("job", job.ID)).
				Warn("resolving update source failed", humane.Zap(rErr)...)
			continue
		}
		if latest == base[mv.Name] {
			continue // already on the latest version
		}
		// Break the revert loop: if we already applied this exact version and
		// the job is no longer on it, Nomad most likely auto-reverted a failed
		// deployment. Re-pushing it would only fail and revert again.
		if prev, ok := u.cache.get(job.key(), mv.Name); ok && prev == latest {
			mChecksSkipped.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "already_applied")))
			log(ctx).With(zap.String("namespace", job.Namespace), zap.String("job", job.ID)).
				Info("skipping update: version already applied and reverted",
					zap.String("variable", mv.Name), zap.String("version", latest))
			continue
		}
		changes[mv.Name] = latest
	}

	if len(changes) == 0 {
		if len(resolveErrs) > 0 {
			outcome = "error"
			return false, errors.Join(resolveErrs...)
		}
		log(ctx).Debug("job is up to date", zap.String("namespace", job.Namespace), zap.String("job", job.ID))
		return false, nil
	}

	if u.dryRun {
		outcome = "dry_run"
		for _, mv := range job.Vars {
			latest, ok := changes[mv.Name]
			if !ok {
				continue
			}
			log(ctx).Info("[dry-run] would update variable",
				zap.String("namespace", job.Namespace), zap.String("job", job.ID),
				zap.String("variable", mv.Name), zap.String("to", latest), zap.String("from", base[mv.Name]))
		}
		return false, nil
	}

	active, depErr := u.nomad.deploymentActive(ctx, job.Namespace, job.ID)
	if depErr != nil {
		outcome = "error"
		span.RecordError(depErr)
		return false, depErr
	}
	if active {
		outcome = "skipped"
		mChecksSkipped.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", "deployment_active")))
		log(ctx).Info("deferring update: deployment in progress",
			zap.String("namespace", job.Namespace), zap.String("job", job.ID))
		return false, nil
	}

	merged := map[string]string{}
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range changes {
		merged[k] = v
	}

	// Re-render the original HCL with the updated variables and register it,
	// carrying the source submission forward so the next cycle reads the new
	// values back from its -var flags. Only the variables change; the job's HCL
	// (and any meta it interpolates from those variables) is preserved as-is.
	rendered, parseErr := u.nomad.parseJob(ctx, job.Namespace, sub.Source, buildVarFile(merged))
	if parseErr != nil {
		outcome = "error"
		span.RecordError(parseErr)
		return false, parseErr
	}

	newSub := &submission{Format: "hcl2", Source: sub.Source, VariableFlags: merged}
	if regErr := u.nomad.registerJob(ctx, job.Namespace, job.ID, rendered, newSub); regErr != nil {
		outcome = "error"
		span.RecordError(regErr)
		return false, regErr
	}

	outcome = "updated"
	mUpdates.Add(ctx, 1)
	for _, mv := range job.Vars {
		if latest, ok := changes[mv.Name]; ok {
			if err := u.cache.set(job.key(), mv.Name, latest); err != nil {
				log(ctx).Warn("could not persist the applied version to the cache", humane.Zap(err)...)
			}
			log(ctx).Info("updated variable",
				zap.String("namespace", job.Namespace), zap.String("job", job.ID),
				zap.String("variable", mv.Name), zap.String("value", latest))
		}
	}
	return true, nil
}

// resolve builds the source for a variable and asks it for the latest value,
// wrapped in a span and duration/failure metrics.
func (u *updater) resolve(ctx context.Context, mv managedVar) (string, error) {
	src, err := u.newSource(mv.Spec)
	if err != nil {
		return "", err
	}

	ctx, span := tracer.Start(ctx, "resolve", trace.WithAttributes(
		attribute.String("autoupdate.variable", mv.Name),
		attribute.String("autoupdate.source.scheme", src.scheme()),
	))
	defer span.End()

	started := time.Now()
	latest, lErr := src.Latest(ctx)
	mSourceResolveDuration.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(
		attribute.String("scheme", src.scheme()),
		attribute.Bool("error", lErr != nil),
	))
	if lErr != nil {
		span.RecordError(lErr)
		span.SetStatus(codes.Error, "resolve failed")
		mSourceFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("scheme", src.scheme())))
		return "", lErr
	}
	span.SetAttributes(attribute.String("autoupdate.resolved", latest))
	return latest, nil
}
