resource "nemau_container" "backups" {
  name           = "backups"
  object_lock    = true
  retention_days = 30
}
