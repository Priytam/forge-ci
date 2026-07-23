# ---- Required / core ----

variable "project_id" {
  description = "GCP project id."
  type        = string
  validation {
    condition     = length(var.project_id) > 0
    error_message = "project_id must be set."
  }
}

variable "region" {
  description = "GCP region."
  type        = string
  validation {
    condition     = length(var.region) > 0
    error_message = "region must be set."
  }
}

variable "name" {
  description = "Name prefix for all resources."
  type        = string
  default     = "forge-ci"
}

# ---- Cluster: create vs bring-your-own ----

variable "create_cluster" {
  description = "Create a GKE cluster. false = install into an existing cluster."
  type        = bool
  default     = true
}

variable "gke_location" {
  description = "GKE location (region for regional cluster). Defaults to region."
  type        = string
  default     = ""
}

variable "machine_type" {
  type    = string
  default = "e2-standard-4"
}

variable "node_count" {
  type    = number
  default = 1
}

variable "min_node_count" {
  type    = number
  default = 1
}

variable "max_node_count" {
  type    = number
  default = 3
}

variable "cluster_deletion_protection" {
  type    = bool
  default = true
}

# Used only when create_cluster=false.
variable "byo_cluster_endpoint" {
  description = "Full https endpoint of the existing cluster."
  type        = string
  default     = ""
}

variable "byo_cluster_ca_certificate" {
  type    = string
  default = ""
}

variable "byo_cluster_token" {
  type      = string
  default   = ""
  sensitive = true
}

# ---- Network: create vs bring-your-own ----

variable "create_vpc" {
  description = "Create a VPC. false = use byo_network_* values."
  type        = bool
  default     = true
}

variable "subnet_cidr" {
  type    = string
  default = "10.30.0.0/20"
}

variable "pods_cidr" {
  type    = string
  default = "10.32.0.0/14"
}

variable "services_cidr" {
  type    = string
  default = "10.36.0.0/20"
}

variable "byo_network_name" {
  type    = string
  default = ""
}

variable "byo_network_id" {
  type    = string
  default = ""
}

variable "byo_subnet_name" {
  type    = string
  default = ""
}

variable "byo_pods_range_name" {
  type    = string
  default = ""
}

variable "byo_services_range_name" {
  type    = string
  default = ""
}

variable "byo_private_vpc_connection" {
  description = "Existing service-networking connection id (for Cloud SQL private IP)."
  type        = string
  default     = null
}

# ---- Database ----

variable "db_tier" {
  type    = string
  default = "db-custom-2-7680"
}

variable "db_availability_type" {
  type    = string
  default = "REGIONAL"
}

variable "db_deletion_protection" {
  type    = bool
  default = true
}

# ---- Redis (log tier) ----

variable "create_redis" {
  description = "Provision managed Memorystore Redis for the log tier. false = bundle the chart's in-cluster Redis instead."
  type        = bool
  default     = true
}

variable "redis_tier" {
  description = "BASIC (single node) or STANDARD_HA (failover replica)."
  type        = string
  default     = "BASIC"
}

variable "redis_memory_gb" {
  type    = number
  default = 1
}

variable "log_backend" {
  description = "LOG_BACKEND: '' (auto), 'redis', or 'postgres'."
  type        = string
  default     = "redis"
}

# ---- Artifacts ----

variable "artifacts_bucket_name" {
  description = "Globally-unique GCS bucket name."
  type        = string
  validation {
    condition     = length(var.artifacts_bucket_name) > 0
    error_message = "artifacts_bucket_name must be set."
  }
}

variable "artifacts_location" {
  description = "Bucket location. Defaults to region."
  type        = string
  default     = ""
}

variable "artifacts_expire_days" {
  type    = number
  default = 0
}

variable "artifacts_force_destroy" {
  type    = bool
  default = false
}

# ---- App / release ----

variable "chart_path" {
  type    = string
  default = "../../helm/forge-ci"
}

variable "namespace" {
  type    = string
  default = "forge-ci"
}

variable "domain" {
  description = "Public hostname for the Forge dashboard/API (ingress host)."
  type        = string
}

variable "ingress_enabled" {
  type    = bool
  default = true
}

variable "ingress_class_name" {
  type    = string
  default = "gce"
}

variable "ingress_tls_enabled" {
  type    = bool
  default = true
}

variable "ingress_annotations" {
  type    = map(string)
  default = {}
}

variable "runner_mode" {
  type    = string
  default = "kubernetes"
}

variable "runner_auth" {
  type    = string
  default = "on"
}

variable "admin_emails" {
  type    = string
  default = ""
}

variable "image_registry" {
  description = "Registry/org prefix for Forge images, e.g. REGION-docker.pkg.dev/PROJECT/forge-ci."
  type        = string
  default     = ""
}

variable "webhook_secret" {
  type      = string
  default   = ""
  sensitive = true
}
