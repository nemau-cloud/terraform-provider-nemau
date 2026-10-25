# An instance created outside Terraform.
data "nemau_instance" "bastion" {
  id = "6f1d2c3b-8a4e-4f7d-9b20-5e3c1a7d9f42"
}

output "bastion_addresses" {
  value = data.nemau_instance.bastion.addresses
}
