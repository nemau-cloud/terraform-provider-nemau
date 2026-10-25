resource "nemau_container_credential" "restore" {
  container = nemau_container.backups.name
  access    = "read_only"
}

# S3 tools address s3_bucket, for example `aws s3 sync s3://<s3_bucket> .`.
output "restore_s3_bucket" {
  value = nemau_container_credential.restore.s3_bucket
}

output "restore_secret_access_key" {
  value     = nemau_container_credential.restore.secret_access_key
  sensitive = true
}
