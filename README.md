# nomad-auto-update

**nomad-auto-update** is a small coordinator that keeps Nomad jobs current: it
watches jobs whose meta declares auto-update variables, resolves the latest
upstream artifact for each on a per-job schedule, and — when a value changes —
re-renders the job's HCL with the updated input variable and registers a new
job version. Nomad's own `update`/`canary` strategy then drives the rollout.

```hcl
variable "grey_version" {
  type    = string
  default = "v2.1.0"
}

job "grey" {
  meta = {
    "autoupdate.grey_version" = "github:SierraSoftworks/grey?prefix=v2."
  }

  # ... image = "ghcr.io/sierrasoftworks/grey:${var.grey_version}"
}
```

When `SierraSoftworks/grey` publishes `v2.3.1`, the coordinator sets
`grey_version` to it and registers a new version of the `grey` job.

## How it works

The coordinator never edits a running job in place. Instead, for each managed
job it:

1. Reads the job's original HCL source and current `-var` values from the
   [job submission API](https://developer.hashicorp.com/nomad/api-docs/jobs#read-job-submission)
   (`GET /v1/job/:id/submission`).
2. Resolves the latest value for each declared source (a GitHub release tag or
   a Docker image digest) and compares it to the current `-var` value.
3. If anything changed — and no deployment is already in progress — re-renders
   the original HCL with the new variables via the
   [parse API](https://developer.hashicorp.com/nomad/api-docs/jobs#parse-job)
   (`POST /v1/jobs/parse`, which applies HCL2 variables server-side) and
   registers the result, carrying the source submission forward so the next
   cycle reads the new values back from its `-var` flags.

Only the input variables change; the job's HCL is otherwise preserved exactly,
including any `meta` you define. Because the change flows through the variables,
a single variable can drive an image tag, an artifact URL, and anything else the
job references — and if you want to record the resolved version in the job's
meta, interpolate the variable there yourself, e.g.
`meta = { grey_version = var.grey_version }`.

## Meta reference

All keys live under the `-meta-prefix` (default `autoupdate`).

| Meta key | Meaning |
|----------|---------|
| `autoupdate.<var>` | Declares that input variable `<var>` is auto-updated from the given source spec. |
| `autoupdate.interval` | Optional per-job check interval (a Go duration such as `6h`). Defaults to `-default-check-interval`. `interval` is therefore a reserved variable name. |

## Update sources

A source spec is `<scheme>:<locator>[?<options>]`.

| Scheme | Example | Resolves to |
|--------|---------|-------------|
| `github` | `github:SierraSoftworks/grey?prefix=v2.` | The newest release tag (e.g. `v2.3.1`). `prefix=` constrains the release line; `prerelease=true` includes pre-releases. Set `GITHUB_TOKEN` to raise the rate limit or reach private repositories; `GITHUB_API_URL` targets GitHub Enterprise. |
| `docker` | `docker:ghcr.io/sierrasoftworks/analytics:latest` | `<tag>@sha256:<digest>`, pinning the current digest while remaining a drop-in for an `image = "name:${var}"` field. Docker Hub short names (`docker:nginx`) are supported. |

## Flags

| Flag | Default | Purpose |
|------|---------|---------|
| `-nomad-addr` | `$NOMAD_ADDR`, else the task API socket, else `http://127.0.0.1:4646` | Nomad API address (`unix://` supported). |
| `-namespace` | `*` | Namespace to scan and operate in (`*` scans all authorized). |
| `-meta-prefix` | `autoupdate` | Job meta key prefix to react to. |
| `-interval` | `5m` | How often to rescan Nomad for jobs to manage. |
| `-default-check-interval` | `1h` | Per-job check interval when a job sets no `autoupdate.interval`. |
| `-concurrency` | `4` | Maximum number of jobs checked concurrently. |
| `-cache-file` | `$NOMAD_ALLOC_DIR/data/applied-versions.json` inside a task, else disabled | Where to persist applied-version state across restarts (see Revert protection). |
| `-once` | off | Run a single discovery and check pass, then exit. |
| `-dry-run` | off | Log the updates that would be applied without registering any job. |
| `-version` | off | Print the version and exit. |

`$NOMAD_TOKEN` is used when set; inside a Nomad task the workload identity token
is provided automatically via the task API socket.

## Deploy

The bundled job runs a single coordinator instance and reaches the local agent
through the task API socket with its workload identity — no API address or token
configuration needed:

```sh
nomad job run jobs/nomad-auto-update.nomad.hcl
```

Upgrades are a re-run away, since the release is a job variable:

```sh
nomad job run -var version=1.0.0 jobs/nomad-auto-update.nomad.hcl
```

### Grant API access (ACL-enabled clusters only)

The coordinator authenticates with the job's workload identity. Attach a policy
granting it the access it needs across the namespaces it manages:

```hcl
# nomad-auto-update-policy.hcl
namespace "*" {
  capabilities = ["list-jobs", "read-job", "submit-job"]
}
```

```sh
nomad acl policy apply \
  -namespace default -job nomad-auto-update \
  nomad-auto-update ./nomad-auto-update-policy.hcl
```

Skip this step entirely if your cluster does not use ACLs.

## Observability

The coordinator emits OpenTelemetry **traces, metrics, and logs**, configured
through the standard `OTEL_*` environment variables. It is **off by default**:
with none of those variables set, nothing tries to reach a collector and it just
logs to stderr. Point it at a collector to turn export on:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
OTEL_SERVICE_NAME=nomad-auto-update   # optional; this is the default
```

Each check runs as a self-contained trace (`autoupdate.*` metrics cover checks,
updates, skips, source-resolution and Nomad request durations). Set
`AUTOUPDATE_LOG_LEVEL` (`debug`/`info`/`warn`/`error`, default `info`) to adjust
console verbosity.

## Notes and constraints

- **The job source must be retained.** Auto-update re-renders the job's HCL, so
  a job registered with `meta.nomad_discard_job_source = true` cannot be
  updated — the coordinator logs an actionable error and skips it.
- **Private registry authentication is a planned addition.** Docker resolution
  currently uses anonymous pulls (which cover public images on Docker Hub,
  GHCR, and similar). GitHub private repositories work today via `GITHUB_TOKEN`.
- **Revert protection.** If an update fails to start and Nomad auto-reverts the
  job, its variables return to their old values while the source still reports
  the newer version. The coordinator remembers the versions it has applied (per
  job variable) and will not push a version it already applied a second time —
  avoiding a revert loop — while still applying any genuinely newer version that
  appears later. This state is persisted to `-cache-file`; the bundled job
  points it at a sticky ephemeral disk so it survives restarts and reschedules.
  With no cache file configured (the default outside a Nomad task) the state is
  in-memory only, so a restart may retry a reverted version once.
- **One instance.** The bundled job runs a single coordinator (`count = 1`);
  updates are compare-and-set idempotent.

## Building

```sh
go build -o nomad-auto-update .
go vet ./... && go test ./...
```

Releases (Linux amd64/arm64 tarballs plus a container image on `ghcr.io`) are
built by the [Release workflow](.github/workflows/release.yml) and tagged
`v<version>`.
