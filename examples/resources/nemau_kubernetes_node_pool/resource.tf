resource "nemau_kubernetes_node_pool" "batch" {
  cluster_name = nemau_kubernetes_cluster.prod.name
  name         = "batch"
  node_class   = "default-worker"

  vcpus   = 8
  ram_mb  = 16384
  disk_gb = 200

  replicas       = 2
  autoscaler_min = 2
  autoscaler_max = 10
}
