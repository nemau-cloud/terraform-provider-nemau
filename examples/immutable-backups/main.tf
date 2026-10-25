# An immutable (WORM) backup container.
#
# object_lock prevents every stored version from being erased during the
# retention period. This protects a backup from ransomware: a compromised S3
# key cannot destroy what is already written. It can add a delete marker, but
# cannot remove the locked version.

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

# GOVERNANCE: the platform can lift the lock on a justified request. The
# default choice for operational backups.
resource "nemau_container" "backups" {
  name           = "backups"
  object_lock    = true
  retention_days = 30
  # retention_mode = "GOVERNANCE"  # the default
}

# COMPLIANCE: nobody can lift the lock before it expires, including Nemau. Use
# it for data whose retention is a legal obligation. A wrong duration cannot be
# corrected, and `terraform destroy` fails while locked versions remain.
resource "nemau_container" "legal_archives" {
  name           = "legal-archives"
  object_lock    = true
  retention_mode = "COMPLIANCE"
  retention_days = 2555 # 7 years

  lifecycle {
    # An accidental destroy cannot succeed before the retention expires anyway;
    # this guard makes the intent explicit and fails the plan rather than the
    # apply.
    prevent_destroy = true
  }
}

# Versioning alone: previous versions are kept and restorable, but a delete
# destroys them. This protects against human error but not against ransomware.
resource "nemau_container" "documents" {
  name       = "documents"
  versioning = true
}

output "backups_immutable" {
  description = "Whether the store enforces WORM on the container"
  value       = nemau_container.backups.immutable
}

output "archives_retention_days" {
  value = nemau_container.legal_archives.retention_days
}
