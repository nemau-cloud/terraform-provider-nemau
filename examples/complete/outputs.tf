output "instance_id" {
  description = "Instance id"
  value       = nemau_instance.web.id
}

output "private_addresses" {
  description = "Private addresses reported by the platform"
  value       = nemau_instance.web.addresses
}

output "public_address" {
  description = "Floating IP associated with the instance"
  value       = nemau_floating_ip.web.address
}

output "data_device" {
  description = "Stable path of the volume inside the guest"
  value       = "/dev/disk/by-id/${nemau_volume_attachment.data.device_id}"
}
