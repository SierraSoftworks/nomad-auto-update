# Runs the nomad-auto-update coordinator as a single cluster-wide service. It
# watches every job whose meta declares auto-update variables and rolls out a
# new job version when an upstream artifact (a GitHub release tag or a Docker
# image digest) changes. See the README for the full setup guide.
#
# The coordinator reaches the local Nomad agent through the task API socket
# using its own workload identity, so no API address or token needs to be
# configured. With ACLs enabled, attach a policy granting list-jobs, read-job,
# parse-job, and submit-job across the namespaces it manages (see the README).

# The release to install, e.g.:
#
#   nomad job run -var version=1.0.0 nomad-auto-update.nomad.hcl
variable "version" {
  type        = string
  default     = "1.0.0"
  description = "nomad-auto-update release to install (GitHub release tag v<version>)."
}

job "nomad-auto-update" {
  type = "service"

  group "auto-update" {
    count = 1

    # Persist the coordinator's applied-version cache across restarts and
    # reschedules. Without it, a version Nomad auto-reverts after a failed
    # deployment could be retried on the next start. sticky keeps the alloc's
    # data on the same client; migrate makes a best-effort move if it relocates.
    # The coordinator auto-detects ${NOMAD_ALLOC_DIR}/data/applied-versions.json.
    ephemeral_disk {
      sticky  = true
      migrate = true
    }

    restart {
      attempts = 5
      interval = "10m"
      delay    = "15s"
      mode     = "delay"
    }

    task "auto-update" {
      driver = "exec"

      # Exposes NOMAD_TOKEN so the task can authenticate to the task API
      # socket (${NOMAD_SECRETS_DIR}/api.sock), which the coordinator
      # auto-detects.
      identity {
        env = true
      }

      env {
        # OpenTelemetry is off unless an exporter is configured. Point it at a
        # collector to turn on traces, metrics, and logs (see "Observability"
        # in the README); uncomment and adjust:
        #
        # OTEL_EXPORTER_OTLP_ENDPOINT = "http://otel-collector.service.consul:4318"
        # OTEL_SERVICE_NAME           = "nomad-auto-update"
      }

      # Optional: a GitHub token (raises the API rate limit and reaches private
      # repositories) and other source credentials. Store them once:
      #
      #   nomad var put nomad/jobs/nomad-auto-update github_token=ghp_...
      #
      template {
        data        = <<-EOT
          {{- with nomadVar "nomad/jobs/nomad-auto-update" }}
          {{- if .github_token }}
          GITHUB_TOKEN={{ .github_token }}
          {{- end }}
          {{- end }}
        EOT
        destination = "secrets/auto-update.env"
        env         = true
      }

      artifact {
        # ${attr.cpu.arch} resolves to amd64 on x86_64 nodes and arm64 on
        # aarch64 nodes; the release version comes from the job's "version"
        # variable above.
        source = "https://github.com/SierraSoftworks/nomad-auto-update/releases/download/v${var.version}/nomad-auto-update_${var.version}_linux_${attr.cpu.arch}.tar.gz"
      }

      config {
        command = "local/nomad-auto-update"
        args = [
          "-namespace=*",
        ]
      }

      # Smoke test from inside the running allocation — resolves every managed
      # job's sources and prints the updates it would apply, without
      # registering anything:
      #
      #   nomad action -job nomad-auto-update -group auto-update -task auto-update dry-run
      action "dry-run" {
        command = "local/nomad-auto-update"
        args    = ["-once", "-dry-run"]
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
