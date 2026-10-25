resource "nemau_dns_zone" "main" {
  name  = "example.com."
  email = "hostmaster@example.com"
}

# Publish this TXT record at the domain's current DNS host. The zone serves
# nothing until nemau_dns_zone_verification has found it.
output "verification_record" {
  value = {
    name  = nemau_dns_zone.main.verification_record_name
    type  = "TXT"
    value = nemau_dns_zone.main.verification_record_value
  }
}
