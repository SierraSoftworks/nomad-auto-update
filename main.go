// Command nomad-auto-update coordinates automatic rollout of new Nomad job
// versions when an upstream artifact changes.
//
// It watches jobs whose meta block declares auto-update variables (e.g.
// "autoupdate.grey_version" = "github:SierraSoftworks/grey?prefix=v2."),
// resolves the latest artifact from the declared source on a per-job schedule,
// and — when the value changed — re-renders the job's HCL with the updated
// HCL2 input variable and registers a new job version. Nomad's own
// update/canary strategy then drives the rollout.
//
// It is designed to run as a Nomad job (see jobs/nomad-auto-update.nomad.hcl),
// reaching the local Nomad agent through the task API socket using its
// workload identity, but works anywhere it can reach a Nomad agent.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	humane "github.com/sierrasoftworks/humane-errors-go"
	"go.uber.org/zap"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	// Logging works from the first line; telemetry (and the log bridge) is
	// attached once the operator's OTEL_* configuration has been read.
	installLogger(nil)

	var (
		nomadAddr       = flag.String("nomad-addr", "", "Nomad API address (default: $NOMAD_ADDR, else the task API socket, else http://127.0.0.1:4646)")
		namespace       = flag.String("namespace", "*", "Nomad namespace to scan and operate in (* scans all authorized namespaces)")
		metaPrefix      = flag.String("meta-prefix", "autoupdate", "job meta key prefix declaring auto-updated variables")
		discoverEvery   = flag.Duration("interval", 5*time.Minute, "how often to rescan Nomad for jobs to manage")
		defaultInterval = flag.Duration("default-check-interval", time.Hour, "default per-job check interval when the job sets no autoupdate.interval meta")
		concurrency     = flag.Int("concurrency", 4, "maximum number of jobs checked concurrently")
		once            = flag.Bool("once", false, "run a single discovery and check pass, then exit")
		dryRun          = flag.Bool("dry-run", false, "log the updates that would be applied without registering any job")
		showVersion     = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// From here on, spans, metrics, and (a bridge to) logs flow to whatever
	// exporters the OTEL_* environment selects; without that configuration
	// this is a no-op and only the console logger runs.
	tel, terr := setupTelemetry(ctx, version)
	if terr != nil {
		log(ctx).Warn("continuing without full telemetry", humane.Zap(terr)...)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tel.shutdown(sctx); err != nil {
			baseConsole.Warn("telemetry shutdown failed", zap.Error(err))
		}
	}()

	addr := resolveNomadAddr(*nomadAddr)
	nomad := newNomadClient(addr, os.Getenv("NOMAD_TOKEN"), *namespace, *metaPrefix)
	up := newUpdater(nomad, *dryRun)
	sched := newScheduler(up, nomad, *discoverEvery, *defaultInterval, *concurrency)

	log(ctx).Info("starting nomad-auto-update",
		zap.String("version", version),
		zap.String("nomad", addr),
		zap.String("namespace", *namespace),
		zap.String("meta_prefix", *metaPrefix),
		zap.Duration("default_interval", *defaultInterval),
		zap.Bool("dry_run", *dryRun),
	)

	if *once {
		sched.runOnce(ctx)
		return 0
	}

	sched.run(ctx)
	log(context.Background()).Info("shutting down")
	return 0
}

// resolveNomadAddr picks the Nomad API address: explicit flag, then
// $NOMAD_ADDR, then the task API unix socket when running inside a Nomad task,
// then the default local agent address.
func resolveNomadAddr(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if env := os.Getenv("NOMAD_ADDR"); env != "" {
		return env
	}
	if dir := os.Getenv("NOMAD_SECRETS_DIR"); dir != "" {
		sock := filepath.Join(dir, "api.sock")
		if _, err := os.Stat(sock); err == nil {
			return "unix://" + sock
		}
	}
	return "http://127.0.0.1:4646"
}
