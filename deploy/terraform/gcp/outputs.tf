output "app_url" {
  description = "Public Forge CI URL."
  value       = module.forge.app_url
}

output "cluster_name" {
  value = var.create_cluster ? module.gke[0].cluster_name : ""
}

output "cluster_endpoint" {
  value = local.cluster_host
}

output "database_private_ip" {
  value = module.database.private_ip_address
}

output "database_connection_name" {
  value = module.database.connection_name
}

output "artifacts_bucket" {
  value = module.artifacts.s3_bucket
}

output "namespace" {
  value = module.forge.namespace
}

output "secret_manager_id" {
  value = module.secret.secret_id
}
