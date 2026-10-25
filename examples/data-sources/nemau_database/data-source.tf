# Read a database that another configuration manages.
data "nemau_database" "shared" {
  name = "shared-db"
}

output "shared_address" {
  value = "${data.nemau_database.shared.address}:${data.nemau_database.shared.port}"
}
