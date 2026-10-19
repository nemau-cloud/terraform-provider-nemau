resource "nemau_health_monitor" "web" {
  pool_id        = nemau_pool.web.id
  type           = "HTTP"
  delay          = 10
  timeout        = 5
  max_retries    = 3
  url_path       = "/health"
  expected_codes = "200-299"
}
