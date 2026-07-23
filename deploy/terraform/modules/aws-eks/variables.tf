variable "name" {
  description = "EKS cluster name / prefix."
  type        = string
}

variable "kubernetes_version" {
  description = "EKS Kubernetes version."
  type        = string
  default     = "1.31"
}

variable "private_subnet_ids" {
  description = "Private subnet ids for nodes."
  type        = list(string)
}

variable "public_subnet_ids" {
  description = "Public subnet ids (for public LB endpoints)."
  type        = list(string)
  default     = []
}

variable "endpoint_public_access" {
  description = "Expose the API server endpoint publicly."
  type        = bool
  default     = true
}

variable "node_instance_types" {
  description = "Instance types for the managed node group."
  type        = list(string)
  default     = ["t3.large"]
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

variable "tags" {
  type    = map(string)
  default = {}
}
