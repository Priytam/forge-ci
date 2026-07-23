# GCS bucket + a dedicated service account with objectAdmin on that bucket, and
# an HMAC key so the app's S3-compatible client (internal/blob) can use GCS via
# the interoperability endpoint storage.googleapis.com.

resource "google_storage_bucket" "this" {
  name                        = var.bucket_name
  location                    = var.location
  force_destroy               = var.force_destroy
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  versioning {
    enabled = var.versioning
  }

  dynamic "lifecycle_rule" {
    for_each = var.expire_days > 0 ? [1] : []
    content {
      action {
        type = "Delete"
      }
      condition {
        age = var.expire_days
      }
    }
  }
}

resource "google_service_account" "this" {
  account_id   = var.service_account_id
  display_name = "Forge CI artifacts"
}

resource "google_storage_bucket_iam_member" "this" {
  bucket = google_storage_bucket.this.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.this.email}"
}

# HMAC key = S3 access/secret pair for the interoperability API.
resource "google_storage_hmac_key" "this" {
  service_account_email = google_service_account.this.email
}
