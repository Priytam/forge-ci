variable "secret_id" {
  description = "Secret Manager secret id."
  type        = string
  default     = "forge-ci-app"
}

variable "generate_runner_token" {
  type    = bool
  default = true
}

variable "webhook_secret" {
  type      = string
  default   = ""
  sensitive = true
}
