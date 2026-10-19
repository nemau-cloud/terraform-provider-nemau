variable "prefix" {
  description = "Prefix for resource names"
  type        = string
  default     = "demo"
}

variable "image_name" {
  description = "Name of the image to boot"
  type        = string
  default     = "ubuntu-24.04"
}

variable "public_key_path" {
  description = "Path to the SSH public key to register"
  type        = string
  default     = "~/.ssh/id_ed25519.pub"
}

variable "admin_cidr" {
  description = "Prefix allowed to reach SSH"
  type        = string
  default     = "0.0.0.0/0"
}
