# A managed Kubernetes cluster and its node pools, then chaining into the
# Kubernetes provider.

terraform {
  required_version = ">= 1.5"
  required_providers {
    nemau = {
      source  = "nemau-cloud/nemau"
      version = "~> 0.1"
    }
  }
}

provider "nemau" {}

data "nemau_kubernetes_versions" "supported" {}

resource "nemau_kubernetes_cluster" "prod" {
  name = "prod"

  # Pinned, rather than data.nemau_kubernetes_versions.supported.latest: on a
  # production cluster, a newly published version must not trigger an upgrade
  # on the next apply. Changing it later upgrades the cluster in place (one
  # guided task: preflight, maintenance window, upgrade); it never replaces it.
  kubernetes_version = "v1.30.4"

  control_plane_vcpus    = 4
  control_plane_ram_mb   = 8192
  control_plane_disk_gb  = 80
  control_plane_replicas = 3 # odd: etcd needs a quorum (1 to 9, 3 by default)

  # The API creates a cluster with at least one node pool. Its replicas and
  # autoscaler bounds change in place; its name, class and node size replace
  # the cluster.
  default_node_pool {
    name       = "workers"
    node_class = "default-worker" # the default, as in the console
    vcpus      = 4
    ram_mb     = 8192
    disk_gb    = 100
    replicas   = 3
  }
}

# Further pools are declared separately, so that adding or resizing one does
# not recreate the cluster.
resource "nemau_kubernetes_node_pool" "batch" {
  cluster_name = nemau_kubernetes_cluster.prod.name
  name         = "batch"
  node_class   = "default-worker"

  vcpus   = 8
  ram_mb  = 16384
  disk_gb = 200

  replicas       = 3
  autoscaler_min = 3
  autoscaler_max = 10
}

# The kubeconfig is stored in Terraform state in clear text and grants
# cluster-admin rights. Encrypt the state, or fetch it outside Terraform with
# `nemau k8s cluster kubeconfig prod --out ~/.kube/nemau-prod.yaml`.
data "nemau_kubeconfig" "prod" {
  cluster_name = nemau_kubernetes_cluster.prod.name
  depends_on   = [nemau_kubernetes_node_pool.batch]
}

output "api_endpoint" {
  description = "API server address"
  value       = nemau_kubernetes_cluster.prod.endpoint
}
