# Referencing the instance makes Terraform detach the volume before it
# destroys the instance.
resource "nemau_volume_attachment" "data" {
  volume_id   = nemau_volume.data.id
  instance_id = nemau_instance.web.id
}
