# HTTPS terminated on a load balancer.
#
# The certificate and its private key are imported once. The platform never
# returns the key, so it is kept in Terraform state; keep that state encrypted.
# The listener decrypts on the load balancer and forwards plain HTTP to the
# pool.

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

variable "vip_subnet_id" {
  type = string
}

resource "nemau_load_balancer_certificate" "www" {
  # Names are unique in a project, and during a renewal the new certificate
  # exists before the old one is deleted: name_prefix generates a new name
  # (prefix, UTC timestamp, random suffix) at each replacement.
  name_prefix = "www-"
  certificate = file("${path.module}/fullchain.pem") # leaf first, then the chain
  private_key = file("${path.module}/privkey.pem")   # RSA 2048-4096 or ECDSA P-256/P-384, no passphrase

  lifecycle {
    # Renewal: replace fullchain.pem and privkey.pem, then apply. The new
    # certificate is imported, the listener is repointed in place, and the old
    # certificate is deleted last, without downtime.
    create_before_destroy = true
  }
}

resource "nemau_load_balancer" "edge" {
  name          = "edge"
  vip_subnet_id = var.vip_subnet_id
}

resource "nemau_pool" "web" {
  name             = "web"
  load_balancer_id = nemau_load_balancer.edge.id
  protocol         = "HTTP"
  lb_algorithm     = "ROUND_ROBIN"
}

resource "nemau_listener" "https" {
  name                = "https"
  load_balancer_id    = nemau_load_balancer.edge.id
  protocol            = "TERMINATED_HTTPS"
  protocol_port       = 443
  default_pool_id     = nemau_pool.web.id
  tls_certificate_ref = nemau_load_balancer_certificate.www.id
}

output "certificate_expires" {
  value = nemau_load_balancer_certificate.www.not_after
}
