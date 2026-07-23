variable "name" {
  description = "Name prefix."
  type        = string
}

variable "vpc_id" {
  type = string
}

variable "subnet_ids" {
  description = "Private subnet ids for the DB subnet group."
  type        = list(string)
}

variable "allowed_cidr_blocks" {
  description = "CIDRs allowed to reach Postgres (typically the VPC CIDR)."
  type        = list(string)
}

variable "engine_version" {
  type    = string
  default = "16.4"
}

variable "instance_class" {
  type    = string
  default = "db.t3.medium"
}

variable "allocated_storage" {
  type    = number
  default = 20
}

variable "max_allocated_storage" {
  type    = number
  default = 100
}

variable "db_name" {
  type    = string
  default = "forge"
}

variable "username" {
  type    = string
  default = "forge"
}

variable "password" {
  description = "DB password. Empty = generate a random one."
  type        = string
  default     = ""
  sensitive   = true
}

variable "multi_az" {
  type    = bool
  default = true
}

variable "backup_retention_period" {
  type    = number
  default = 7
}

variable "deletion_protection" {
  type    = bool
  default = true
}

variable "skip_final_snapshot" {
  type    = bool
  default = false
}

variable "sslmode" {
  description = "sslmode appended to the DSN."
  type        = string
  default     = "require"
}

variable "tags" {
  type    = map(string)
  default = {}
}
