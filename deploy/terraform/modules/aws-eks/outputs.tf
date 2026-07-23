output "cluster_name" {
  value = aws_eks_cluster.this.name
}

output "cluster_endpoint" {
  description = "EKS API server endpoint."
  value       = aws_eks_cluster.this.endpoint
}

output "cluster_ca_certificate" {
  description = "Base64 cluster CA certificate."
  value       = aws_eks_cluster.this.certificate_authority[0].data
}

output "cluster_token" {
  description = "Short-lived auth token for the kubernetes/helm providers."
  value       = data.aws_eks_cluster_auth.this.token
  sensitive   = true
}
