output "app_url" {
  description = "Public Forge CI URL."
  value       = module.forge.app_url
}

output "cluster_name" {
  description = "EKS cluster name (empty for bring-your-own)."
  value       = var.create_cluster ? module.eks[0].cluster_name : ""
}

output "cluster_endpoint" {
  value = local.cluster_endpoint
}

output "database_endpoint" {
  description = "RDS endpoint host."
  value       = module.database.address
}

output "artifacts_bucket" {
  value = module.artifacts.s3_bucket
}

output "namespace" {
  value = module.forge.namespace
}

output "secrets_manager_arn" {
  value = module.secret.secret_arn
}
