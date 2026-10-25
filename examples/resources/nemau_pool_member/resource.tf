resource "nemau_pool_member" "web" {
  pool_id       = nemau_pool.web.id
  name          = "web"
  address       = nemau_instance.web.addresses[0]
  protocol_port = 80
  subnet_id     = nemau_subnet.app.id
}
