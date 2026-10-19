resource "nemau_load_balancer" "edge" {
  name          = "edge"
  vip_subnet_id = nemau_subnet.app.id
  flavor        = "lb-ha"
}
