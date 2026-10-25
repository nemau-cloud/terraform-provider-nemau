resource "nemau_subnet" "app" {
  network_id = nemau_network.app.id
  name       = "app"
  cidr       = "10.0.1.0/24"
}

# An IPv6 subnet makes the network dual-stack. The platform assigns the /64,
# and the network needs its IPv4 subnet first.
resource "nemau_subnet" "app_v6" {
  network_id = nemau_network.app.id
  name       = "app-v6"
  ip_version = 6
  depends_on = [nemau_subnet.app]
}
