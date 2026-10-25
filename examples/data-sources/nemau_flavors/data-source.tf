# Requires the compute-flavors organization feature.
data "nemau_flavors" "all" {}

output "flavor_ids" {
  value = { for flavor in data.nemau_flavors.all.flavors : flavor.name => flavor.id }
}
