variable "name" {
  description = "Name prefix for IAM resources."
  type        = string
}

variable "bucket_name" {
  description = "Globally-unique S3 bucket name for artifacts."
  type        = string
}

variable "region" {
  description = "Bucket region (used to build the S3 endpoint host)."
  type        = string
}

variable "versioning" {
  type    = bool
  default = false
}

variable "expire_days" {
  description = "Lifecycle expiration for artifacts in days. 0 disables."
  type        = number
  default     = 0
}

variable "force_destroy" {
  description = "Allow deleting a non-empty bucket on destroy."
  type        = bool
  default     = false
}

variable "tags" {
  type    = map(string)
  default = {}
}
