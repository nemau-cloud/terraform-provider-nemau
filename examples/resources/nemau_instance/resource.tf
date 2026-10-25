resource "nemau_instance" "web" {
  name     = "web"
  image_id = one(data.nemau_images.ubuntu.images).id

  # A custom size. With the compute-flavors organization feature, flavor_id
  # can replace these three arguments.
  vcpus   = 2
  ram_mb  = 4096
  disk_gb = 40

  network_id         = nemau_network.app.id
  security_group_ids = [nemau_security_group.web.id]
  key_name           = nemau_keypair.deploy.name

  # The instance takes an address in the subnet, and no argument references
  # it, so the dependency is declared.
  depends_on = [nemau_subnet.app]
}
