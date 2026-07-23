variable "name" {
  description = "Name prefix."
  type        = string
}

variable "vpc_id" {
  type = string
}

variable "subnet_ids" {
  description = "Private subnet ids for the ElastiCache subnet group."
  type        = list(string)
}

variable "allowed_cidr_blocks" {
  description = "CIDRs allowed to reach Redis (typically the VPC CIDR)."
  type        = list(string)
}

variable "engine_version" {
  type    = string
  default = "7.1"
}

variable "node_type" {
  type    = string
  default = "cache.t4g.small"
}

variable "num_cache_clusters" {
  description = "Number of nodes (1 = single node; >1 enables automatic failover + multi-AZ)."
  type        = number
  default     = 1
}

variable "snapshot_retention_limit" {
  description = "Days of snapshots to retain. 0 disables (the log buffer is ephemeral)."
  type        = number
  default     = 0
}

variable "transit_encryption_enabled" {
  type    = bool
  default = true
}

variable "at_rest_encryption_enabled" {
  type    = bool
  default = true
}

variable "auth_token" {
  description = "Redis AUTH token (requires transit_encryption_enabled). Empty = no auth."
  type        = string
  default     = ""
  sensitive   = true
}

variable "tags" {
  type    = map(string)
  default = {}
}
