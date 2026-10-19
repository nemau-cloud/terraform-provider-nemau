# A full stack: network, subnet, filtering, instance and public IP.
#
# The organization comes from the credential. The project comes from the
# provider's `project` argument, or from NEMAU_PROJECT_ID. See the project
# selection guide in docs/guides, and examples/multi-project for two projects
# with one credential.

terraform {
  required_version = ">= 1.5"
  required_providers {
    nemau = {
      source  = "nemau-cloud/nemau"
      version = "~> 0.1"
    }
  }
}

# Credentials read from NEMAU_ACCESS_KEY_ID / NEMAU_ACCESS_KEY_SECRET.
provider "nemau" {}

data "nemau_images" "ubuntu" {
  name = var.image_name
}

resource "nemau_network" "app" {
  name        = "${var.prefix}-net"
  description = "Application network managed by Terraform"
}

resource "nemau_subnet" "app" {
  network_id  = nemau_network.app.id
  name        = "${var.prefix}-subnet"
  cidr        = "10.42.0.0/24"
  dns_servers = ["1.1.1.1", "9.9.9.9"]
}

resource "nemau_security_group" "web" {
  name        = "${var.prefix}-web"
  description = "SSH from the admin range, HTTP/HTTPS open"
}

resource "nemau_security_group_rule" "ssh" {
  security_group_id = nemau_security_group.web.id
  direction         = "ingress"
  protocol          = "tcp"
  port_range_min    = 22
  port_range_max    = 22
  # Narrow this to your admin prefixes: 0.0.0.0/0 exposes SSH to the whole
  # internet.
  remote_ip_prefix = var.admin_cidr
}

resource "nemau_security_group_rule" "https" {
  security_group_id = nemau_security_group.web.id
  direction         = "ingress"
  protocol          = "tcp"
  port_range_min    = 443
  port_range_max    = 443
  remote_ip_prefix  = "0.0.0.0/0"
}

resource "nemau_keypair" "deploy" {
  name       = "${var.prefix}-deploy"
  public_key = file(var.public_key_path)
}

resource "nemau_instance" "web" {
  name     = "${var.prefix}-web"
  image_id = one(data.nemau_images.ubuntu.images).id

  # A custom size. With the compute-flavors organization feature, replace these
  # three lines with flavor_id.
  vcpus   = 2
  ram_mb  = 4096
  disk_gb = 40

  network_id         = nemau_network.app.id
  security_group_ids = [nemau_security_group.web.id]
  key_name           = nemau_keypair.deploy.name

  # The subnet has to exist before the instance takes an address in it. No
  # argument references the subnet, so the dependency is declared explicitly.
  depends_on = [nemau_subnet.app]
}

resource "nemau_volume" "data" {
  name = "${var.prefix}-data"
  size = 100
}

# Referencing the instance here orders `terraform destroy`: the volume is
# detached first, because the platform refuses to delete an instance that still
# has a volume attached.
resource "nemau_volume_attachment" "data" {
  volume_id   = nemau_volume.data.id
  instance_id = nemau_instance.web.id
}

resource "nemau_floating_ip" "web" {
  description = "Public IP for ${var.prefix}-web"
}

resource "nemau_floating_ip_association" "web" {
  floating_ip_id = nemau_floating_ip.web.id
  target_type    = "instance"
  target_id      = nemau_instance.web.id
}
