resource "nemau_pool" "web" {
  load_balancer_id = nemau_load_balancer.edge.id
  name             = "web"
  protocol         = "HTTP"
  lb_algorithm     = "ROUND_ROBIN"
}
