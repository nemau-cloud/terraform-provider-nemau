resource "nemau_kubernetes_cluster" "prod" {
  name               = "prod"
  kubernetes_version = "v1.30.4"

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
