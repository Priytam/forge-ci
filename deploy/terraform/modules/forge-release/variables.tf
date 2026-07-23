variable "release_name" {
  description = "Helm release name."
  type        = string
  default     = "forge"
}

variable "namespace" {
  description = "Kubernetes namespace to install Forge CI into."
  type        = string
  default     = "forge-ci"
}

variable "create_namespace" {
  description = "Create the namespace (false when it already exists)."
  type        = bool
  default     = true
}

variable "chart_path" {
  description = "Path to the forge-ci Helm chart directory."
  type        = string
}

variable "chart_version" {
  description = "Optional chart version (ignored for local path installs)."
  type        = string
  default     = null
}

# --- Cloud-derived wiring ---------------------------------------------------

variable "database_url" {
  description = "Full Postgres DSN for the managed database."
  type        = string
  sensitive   = true
}

variable "forge_secret_key" {
  description = "base64 32-byte AES-256-GCM key (FORGE_SECRET_KEY)."
  type        = string
  sensitive   = true
}

variable "runner_token" {
  description = "Runner bearer token (used when runner_auth=on)."
  type        = string
  sensitive   = true
  default     = ""
}

variable "webhook_secret" {
  description = "GitHub webhook HMAC secret (WEBHOOK_SECRET)."
  type        = string
  sensitive   = true
  default     = ""
}

variable "artifact_store" {
  description = "local | s3"
  type        = string
  default     = "s3"
  validation {
    condition     = contains(["local", "s3"], var.artifact_store)
    error_message = "artifact_store must be 'local' or 's3'."
  }
}

variable "s3" {
  description = "S3-compatible artifact store config (used when artifact_store=s3)."
  type = object({
    endpoint   = string
    bucket     = string
    region     = optional(string, "")
    use_ssl    = optional(bool, true)
    access_key = string
    secret_key = string
  })
  default   = null
  sensitive = true
}

# --- App config -------------------------------------------------------------

variable "image_registry" {
  description = "Registry/org prefix for Forge images (image.registry)."
  type        = string
  default     = ""
}

variable "runner_mode" {
  description = "shell | docker | kubernetes"
  type        = string
  default     = "kubernetes"
}

variable "runner_auth" {
  description = "on | off — require runner bearer tokens."
  type        = string
  default     = "on"
}

variable "admin_emails" {
  description = "Comma-separated platform-admin emails."
  type        = string
  default     = ""
}

variable "postgresql_enabled" {
  description = "Use the chart's bundled Postgres. Keep false when using a managed DB."
  type        = bool
  default     = false
}

variable "ingress" {
  description = "Ingress configuration."
  type = object({
    enabled     = optional(bool, true)
    class_name  = optional(string, "")
    host        = optional(string, "")
    tls_enabled = optional(bool, true)
    tls_secret  = optional(string, "forge-ci-tls")
    annotations = optional(map(string), {})
  })
  default = {}
}

variable "extra_values" {
  description = "Extra Helm values merged last (highest precedence), as a map."
  type        = any
  default     = {}
}

variable "wait" {
  description = "Wait for the release to become ready."
  type        = bool
  default     = true
}
