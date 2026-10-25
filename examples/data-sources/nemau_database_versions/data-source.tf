# The PostgreSQL versions the region offers. Pin one explicitly in production:
# the list changes as the platform adds versions.
data "nemau_database_versions" "available" {}

output "database_versions" {
  value = data.nemau_database_versions.available.versions
}
