# zone_id comes from the verification rather than from the zone, so the
# record set is created once the zone is verified.
resource "nemau_dns_record_set" "www" {
  zone_id = nemau_dns_zone_verification.main.zone_id
  name    = "www.example.com."
  type    = "A"
  records = ["203.0.113.10"]
}
