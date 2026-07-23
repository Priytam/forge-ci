# ---- Required / core ----

variable "region" {
  description = "AWS region."
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

variable "tags" {
  description = "Tags applied to created AWS resources."
  type        = map(string)
  default     = { app = "forge-ci" }
}

# ---- Cluster: create vs bring-your-own ----

variable "create_cluster" {
  description = "Create an EKS cluster. false = install into an existing cluster (byo_*)."
  type        = bool
  default     = true
}

variable "kubernetes_version" {
  type    = string
  default = "1.31"
}

variable "node_instance_types" {
  type    = list(string)
  default = ["t3.large"]
}

variable "node_desired_size" {
  type    = number
  default = 2
}

variable "node_min_size" {
  type    = number
  default = 2
}

variable "node_max_size" {
  type    = number
  default = 4
}

# Used only when create_cluster=false.
variable "byo_cluster_endpoint" {
  type    = string
  default = ""
}

variable "byo_cluster_ca_certificate" {
  description = "Base64-encoded CA cert for the existing cluster."
  type        = string
  default     = ""
}

variable "byo_cluster_token" {
  type      = string
  default   = ""
  sensitive = true
}

# ---- Network: create vs bring-your-own ----

variable "create_vpc" {
  description = "Create a VPC. false = use byo_vpc_id / byo_*_subnet_ids."
  type        = bool
  default     = true
}

variable "vpc_cidr" {
  type    = string
  default = "10.20.0.0/16"
}

variable "az_count" {
  type    = number
  default = 2
}

variable "byo_vpc_id" {
  type    = string
  default = ""
}

variable "byo_vpc_cidr" {
  description = "CIDR allowed to reach RDS when create_vpc=false."
  type        = string
  default     = ""
}

variable "byo_private_subnet_ids" {
  type    = list(string)
  default = []
}

variable "byo_public_subnet_ids" {
  type    = list(string)
  default = []
}

# ---- Database ----

variable "db_instance_class" {
  type    = string
  default = "db.t3.medium"
}

variable "db_multi_az" {
  type    = bool
  default = true
}

variable "db_deletion_protection" {
  type    = bool
  default = true
}

variable "db_skip_final_snapshot" {
  type    = bool
  default = false
}

# ---- Artifacts ----

variable "artifacts_bucket_name" {
  description = "Globally-unique S3 bucket name for artifacts."
  type        = string
  validation {
    condition     = length(var.artifacts_bucket_name) > 0
    error_message = "artifacts_bucket_name must be set."
  }
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
  description = "Path to the forge-ci Helm chart."
  type        = string
  default     = "../../helm/forge-ci"
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
  default = "alb"
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
  description = "shell | docker | kubernetes"
  type        = string
  default     = "kubernetes"
}

variable "runner_auth" {
  description = "on | off"
  type        = string
  default     = "on"
}

variable "admin_emails" {
  type    = string
  default = ""
}

variable "image_registry" {
  description = "Registry/org prefix for Forge images (image.registry)."
  type        = string
  default     = ""
}

variable "webhook_secret" {
  type      = string
  default   = ""
  sensitive = true
}
