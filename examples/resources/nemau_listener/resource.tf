resource "nemau_listener" "https" {
  load_balancer_id    = nemau_load_balancer.edge.id
  name                = "https"
  protocol            = "TERMINATED_HTTPS"
  protocol_port       = 443
  default_pool_id     = nemau_pool.web.id
  tls_certificate_ref = nemau_load_balancer_certificate.www.id
}
