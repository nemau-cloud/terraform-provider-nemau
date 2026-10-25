# S3 keys scoped to one container.
#
# The secret of a key is stored in the Terraform state. The platform returns it
# once and cannot recover it, so the state holds the only copy. Use an
# encrypted remote backend and restrict who can read the state.
# `sensitive = true` hides a value from plan output; it does not keep it out of
# the state file.

terraform {
  required_version = ">= 1.5"
  required_providers {
    nemau = {
      source  = "nemau-cloud/nemau"
      version = "~> 0.1"
    }
  }
}

provider "nemau" {}

resource "nemau_container" "backups" {
  name = "backups"
}

# A read-write key for the backup job, with no expiry.
resource "nemau_container_credential" "backup_job" {
  container = nemau_container.backups.name
  access    = "read_write"
}

# A read-only key for a restore tool, valid until the end of the year.
resource "nemau_container_credential" "restore" {
  container  = nemau_container.backups.name
  access     = "read_only"
  expires_at = "2026-12-31T23:59:00Z"
}

output "backup_job_access_key_id" {
  value = nemau_container_credential.backup_job.access_key_id
}

output "backup_job_secret_access_key" {
  value     = nemau_container_credential.backup_job.secret_access_key
  sensitive = true
}

# The bucket S3 tools address. It differs from the container name: the store
# knows each container under an opaque name, and the key's policy covers that
# name. For example `aws s3 sync s3://<this> ./backup` or
# `rclone sync remote:<this> ./backup`.
output "s3_bucket" {
  value = nemau_container_credential.backup_job.s3_bucket
}

output "s3_region" {
  value = nemau_container_credential.backup_job.region
}

# Empty in an environment without a reachable S3 endpoint.
output "s3_endpoint" {
  value = nemau_container_credential.backup_job.endpoint
}
