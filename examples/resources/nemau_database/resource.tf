resource "nemau_database" "orders" {
  name               = "orders-db"
  version            = "16"
  flavor             = "db-small"
  storage_gb         = 20
  subnet_id          = nemau_subnet.app.id
  admin_username     = "appadmin"
  security_group_ids = [nemau_security_group.db.id]

  backup_retention_days = 14

  maintenance_window = {
    weekday = 0 # Sunday, UTC
    hour    = 3
  }
}

# The administrator password is issued once, at creation. It lives in the state.
output "orders_password" {
  value     = nemau_database.orders.admin_password
  sensitive = true
}

output "orders_address" {
  value = "${nemau_database.orders.address}:${nemau_database.orders.port}"
}

# Rotate the password on the next apply by changing the trigger.
resource "nemau_database" "rotated" {
  name           = "reports-db"
  version        = "16"
  flavor         = "db-small"
  storage_gb     = 10
  subnet_id      = nemau_subnet.app.id
  admin_username = "reports"

  password_rotation_trigger = "2026-10-01"
}

# A point-in-time restore is a new database. Changing restore_from replaces it.
resource "nemau_database" "orders_restored" {
  name           = "orders-db-restored"
  flavor         = "db-small"
  storage_gb     = 20
  subnet_id      = nemau_subnet.app.id
  admin_username = "appadmin"

  restore_from {
    source_database_id = nemau_database.orders.id
    target_time        = "2026-10-20T09:30:00Z"
  }
}
