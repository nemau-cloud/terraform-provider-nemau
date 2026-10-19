data "nemau_networks" "all" {}

# The region's external network, to allocate a floating IP from it explicitly.
resource "nemau_floating_ip" "explicit" {
  external_network_id = one([for network in data.nemau_networks.all.networks : network.id if network.external])
}
