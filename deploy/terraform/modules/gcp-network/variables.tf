variable "name" {
  description = "Name prefix."
  type        = string
}

variable "region" {
  type = string
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
