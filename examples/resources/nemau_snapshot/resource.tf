resource "nemau_snapshot" "web" {
  instance_id = nemau_instance.web.id
  name        = "web-before-upgrade"
}
