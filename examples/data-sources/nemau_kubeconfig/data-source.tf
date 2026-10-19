# The kubeconfig grants cluster-admin rights and is stored in the state in
# clear text. Keep the state encrypted.
data "nemau_kubeconfig" "prod" {
  cluster_name = nemau_kubernetes_cluster.prod.name
}

output "kubeconfig" {
  value     = data.nemau_kubeconfig.prod.raw
  sensitive = true
}
