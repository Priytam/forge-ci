variable "name" {
  description = "Name prefix."
  type        = string
}

variable "region" {
  type = string
}

variable "authorized_network" {
  description = "VPC network id/self-link the instance is reachable from (private IP)."
  type        = string
}

variable "private_vpc_connection" {
  description = "Service networking connection to depend on for PRIVATE_SERVICE_ACCESS."
  type        = any
  default     = null
}

variable "tier" {
  description = "BASIC (single node) or STANDARD_HA (failover replica)."
  type        = string
  default     = "BASIC"
}

variable "memory_size_gb" {
  type    = number
  default = 1
}

variable "redis_version" {
  type    = string
  default = "REDIS_7_0"
}

variable "transit_encryption_mode" {
  description = "DISABLED or SERVER_AUTHENTICATION (in-transit TLS)."
  type        = string
  default     = "DISABLED"
}
