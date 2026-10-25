# The flavors the region offers. Pick one of their names as the flavor of
# nemau_database. The provider does not check the choice at plan time.
data "nemau_database_flavors" "all" {}

output "database_flavors" {
  value = [for f in data.nemau_database_flavors.all.flavors : "${f.name} (${f.vcpus} vCPU, ${f.memory_mb} MB)"]
}
