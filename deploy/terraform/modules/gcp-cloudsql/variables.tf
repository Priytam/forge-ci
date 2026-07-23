variable "name" {
  type = string
}

variable "region" {
  type = string
}

variable "private_network_id" {
  description = "VPC self_link/id for the private IP."
  type        = string
}

variable "private_vpc_connection" {
  description = "Service networking connection to depend on (from gcp-network)."
  type        = any
  default     = null
}

variable "database_version" {
  type    = string
  default = "POSTGRES_16"
}

variable "tier" {
  type    = string
  default = "db-custom-2-7680"
}

variable "availability_type" {
  description = "ZONAL or REGIONAL (HA)."
  type        = string
  default     = "REGIONAL"
}

variable "disk_size" {
  type    = number
  default = 20
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
  description = "DB password. Empty = generate."
  type        = string
  default     = ""
  sensitive   = true
}

variable "sslmode" {
  type    = string
  default = "disable"
}

variable "deletion_protection" {
  type    = bool
  default = true
}
