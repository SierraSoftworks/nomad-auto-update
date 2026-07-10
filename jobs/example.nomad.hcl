# Example job showing how to opt into automatic updates.
#
# The coordinator reads the meta keys below, resolves each source, and — when a
# value changes — re-renders this job with the updated input variable and
# registers a new version. The variables are ordinary HCL2 input variables, so
# the rest of the job references them however it likes.
#
# Run it with:  nomad job run jobs/example.nomad.hcl

variable "grey_version" {
  type    = string
  default = "v2.1.0"
}

variable "analytics_version" {
  type    = string
  # A bare tag is a valid default; once resolved the coordinator pins it to a
  # digest as "latest@sha256:..." while keeping this variable a drop-in for the
  # image reference below.
  default = "latest"
}

job "example" {
  meta = {
    # Track the newest v2.x release of SierraSoftworks/grey; the variable holds
    # the release tag (e.g. v2.3.1).
    "autoupdate.grey_version" = "github:SierraSoftworks/grey?prefix=v2."

    # Track the current digest behind ghcr.io/sierrasoftworks/analytics:latest;
    # the variable holds "latest@sha256:<digest>".
    "autoupdate.analytics_version" = "docker:ghcr.io/sierrasoftworks/analytics:latest"

    # Optional: check this job every 6 hours instead of the service default.
    "autoupdate.interval" = "6h"
  }

  group "app" {
    task "grey" {
      driver = "docker"

      config {
        image = "ghcr.io/sierrasoftworks/grey:${var.grey_version}"
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }

    task "analytics" {
      driver = "docker"

      config {
        image = "ghcr.io/sierrasoftworks/analytics:${var.analytics_version}"
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
