variable "bucket_name" {
  description = "Globally-unique GCS bucket name."
  type        = string
}

variable "location" {
  description = "Bucket location (region or multi-region)."
  type        = string
}

variable "service_account_id" {
  description = "Service account id (account_id part)."
  type        = string
  default     = "forge-ci-artifacts"
}

variable "versioning" {
  type    = bool
  default = false
}

variable "expire_days" {
  description = "Object age (days) before deletion. 0 disables."
  type        = number
  default     = 0
}

variable "force_destroy" {
  type    = bool
  default = false
}
