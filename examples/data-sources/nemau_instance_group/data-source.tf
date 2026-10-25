# Read an instance group that is managed elsewhere.
data "nemau_instance_group" "web" {
  id = "9b1e2f3a-4c5d-4e6f-8a7b-1c2d3e4f5a6b"
}

output "web_instance_ids" {
  value = data.nemau_instance_group.web.instance_ids
}
