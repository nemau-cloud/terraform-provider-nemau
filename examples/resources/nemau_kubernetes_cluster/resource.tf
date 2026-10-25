resource "nemau_kubernetes_cluster" "prod" {
  name               = "prod"
  kubernetes_version = "v1.30.4"

  # The project network and the subnet the nodes live in. Placeholder UUIDs:
  # use the IDs of your own network and subnet.
  network_id = "3f2b8c1e-5d4a-4e7b-9c1d-0a1b2c3d4e5f"
  subnet_id  = "7a9e0b2c-1d3f-4a5b-8c6d-2e4f6a8b0c1d"

  control_plane_vcpus    = 4
  control_plane_ram_mb   = 8192
  control_plane_disk_gb  = 80
  control_plane_replicas = 3

  default_node_pool {
    name     = "workers"
    vcpus    = 4
    ram_mb   = 8192
    disk_gb  = 100
    replicas = 3
  }
}
