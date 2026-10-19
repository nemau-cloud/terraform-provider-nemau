data "nemau_regions" "enabled" {}

output "region_slugs" {
  value = data.nemau_regions.enabled.regions[*].slug
}
