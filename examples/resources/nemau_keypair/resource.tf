resource "nemau_keypair" "deploy" {
  name       = "deploy"
  public_key = file(pathexpand("~/.ssh/id_ed25519.pub"))
}
