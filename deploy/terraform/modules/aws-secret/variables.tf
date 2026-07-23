variable "secret_name" {
  description = "Secrets Manager secret name."
  type        = string
  default     = "forge-ci/app"
}

variable "generate_runner_token" {
  description = "Generate a runner bearer token (for RUNNER_AUTH=on)."
  type        = bool
  default     = true
}

variable "webhook_secret" {
  description = "Optional GitHub webhook HMAC secret to store."
  type        = string
  default     = ""
  sensitive   = true
}

variable "recovery_window_in_days" {
  description = "Deleted-secret recovery window. 0 = delete immediately."
  type        = number
  default     = 7
}

variable "tags" {
  type    = map(string)
  default = {}
}
