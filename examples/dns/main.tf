# A DNS zone, the proof that you own its domain, and a record in it.
#
# A new zone is not served until the platform has found a TXT record at the
# domain's current DNS host, the one the registrar points to today. The zone
# provides that record in verification_record_name and
# verification_record_value. nemau_dns_zone_verification then waits for it to
# be visible, and record sets are created after the verification.
#
# Without a Terraform provider for the current DNS host, apply in two steps:
#
#     terraform apply -target=nemau_dns_zone.main
#     terraform output verification_record   # publish it by hand
#     terraform apply
#
# Once the zone is active, delegate the domain to the zone's NS records at the
# registrar.

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

variable "domain" {
  description = "The domain to host, without the trailing dot"
  type        = string
  default     = "example.com"
}

resource "nemau_dns_zone" "main" {
  name  = "${var.domain}."
  email = "hostmaster@${var.domain}"
}

# When the current DNS host has a Terraform provider, declare the TXT record
# there from the two attributes, in the same configuration. For example, with a
# host that accepts RFC 2136 updates (hashicorp/dns):
#
# resource "dns_txt_record_set" "nemau_challenge" {
#   zone = "${var.domain}."
#   name = trimsuffix(nemau_dns_zone.main.verification_record_name, ".${var.domain}.")
#   txt  = [nemau_dns_zone.main.verification_record_value]
#   ttl  = 300
# }
#
# The two attributes keep their value once the zone is verified, even though
# the API stops returning them, so that record does not change afterwards.

resource "nemau_dns_zone_verification" "main" {
  zone_id = nemau_dns_zone.main.id

  # Checked again every 30 seconds until the record is visible. Raise it for a
  # DNS host that is slow to publish.
  timeout = "1h"

  # depends_on = [dns_txt_record_set.nemau_challenge]
}

# zone_id comes from the verification resource rather than from the zone: the
# API refuses record sets on a pending zone, and this reference makes Terraform
# wait for the verification.
resource "nemau_dns_record_set" "www" {
  zone_id = nemau_dns_zone_verification.main.zone_id
  name    = "www.${var.domain}."
  type    = "A"
  records = ["203.0.113.10"]
}

output "verification_record" {
  description = "The TXT record to publish at the domain's current DNS host"
  value = {
    name  = nemau_dns_zone.main.verification_record_name
    type  = "TXT"
    value = nemau_dns_zone.main.verification_record_value
  }
}
