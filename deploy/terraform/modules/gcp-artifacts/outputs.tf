# Maps onto the chart's config.artifacts.s3.* / secrets.s3SecretKey. GCS in
# interop mode speaks S3 at storage.googleapis.com.

output "s3_endpoint" {
  value = "storage.googleapis.com"
}

output "s3_bucket" {
  value = google_storage_bucket.this.name
}

output "s3_region" {
  description = "GCS interop uses 'auto' as the region."
  value       = "auto"
}

output "s3_access_key" {
  value     = google_storage_hmac_key.this.access_id
  sensitive = true
}

output "s3_secret_key" {
  value     = google_storage_hmac_key.this.secret
  sensitive = true
}

output "service_account_email" {
  value = google_service_account.this.email
}
