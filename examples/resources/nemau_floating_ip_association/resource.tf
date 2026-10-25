resource "nemau_floating_ip_association" "web" {
  floating_ip_id = nemau_floating_ip.web.id
  target_type    = "instance"
  target_id      = nemau_instance.web.id
}
