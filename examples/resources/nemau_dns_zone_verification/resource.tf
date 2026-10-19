resource "nemau_dns_zone_verification" "main" {
  zone_id = nemau_dns_zone.main.id
  timeout = "1h"
}
