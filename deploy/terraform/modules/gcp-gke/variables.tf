variable "name" {
  description = "GKE cluster name."
  type        = string
}

variable "location" {
  description = "Region (regional cluster) or zone (zonal cluster)."
  type        = string
}

variable "network_name" {
  type = string
}

variable "subnet_name" {
  type = string
}

variable "pods_range_name" {
  type = string
}

variable "services_range_name" {
  type = string
}

variable "release_channel" {
  type    = string
  default = "REGULAR"
}

variable "machine_type" {
  type    = string
  default = "e2-standard-4"
}

variable "disk_size_gb" {
  type    = number
  default = 50
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

variable "deletion_protection" {
  type    = bool
  default = true
}
