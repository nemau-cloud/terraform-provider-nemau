resource "nemau_security_group" "web" {
  name        = "web"
  description = "HTTPS from anywhere"
}
