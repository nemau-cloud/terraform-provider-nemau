resource "nemau_load_balancer_certificate" "www" {
  name_prefix = "www-"
  certificate = file("${path.module}/fullchain.pem")
  private_key = file("${path.module}/privkey.pem")

  # To renew, replace the two files and apply. The new certificate is created
  # first, the listener moves to it, and the old one is deleted.
  lifecycle {
    create_before_destroy = true
  }
}
