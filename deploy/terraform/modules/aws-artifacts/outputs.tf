# These map directly onto the chart's config.artifacts.s3.* / secrets.s3SecretKey
# and the app's S3_* env (internal/blob).

output "s3_endpoint" {
  description = "S3 endpoint host (no scheme), regional."
  value       = "s3.${var.region}.amazonaws.com"
}

output "s3_bucket" {
  value = aws_s3_bucket.this.bucket
}

output "s3_region" {
  value = var.region
}

output "s3_access_key" {
  value     = aws_iam_access_key.this.id
  sensitive = true
}

output "s3_secret_key" {
  value     = aws_iam_access_key.this.secret
  sensitive = true
}

output "bucket_arn" {
  value = aws_s3_bucket.this.arn
}
