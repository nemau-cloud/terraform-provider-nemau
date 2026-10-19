data "nemau_kubernetes_versions" "supported" {}

output "kubernetes_versions" {
  value = data.nemau_kubernetes_versions.supported.versions
}
