data "nemau_quotas" "current" {}

# Room left in each dimension, in that dimension's unit.
output "quota_available" {
  value = { for dimension in data.nemau_quotas.current.dimensions : dimension.name => dimension.available }
}
