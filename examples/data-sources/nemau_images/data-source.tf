data "nemau_images" "ubuntu" {
  name = "ubuntu-24.04"
}

# one() fails when the name matches no image or several.
output "ubuntu_image_id" {
  value = one(data.nemau_images.ubuntu.images).id
}
