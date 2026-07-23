output "cluster_name" {
  value = google_container_cluster.this.name
}

output "cluster_endpoint" {
  description = "GKE API server endpoint (host)."
  value       = google_container_cluster.this.endpoint
}

output "cluster_ca_certificate" {
  description = "Base64 cluster CA certificate."
  value       = google_container_cluster.this.master_auth[0].cluster_ca_certificate
}

output "cluster_token" {
  description = "Short-lived OAuth access token for the kubernetes/helm providers."
  value       = data.google_client_config.this.access_token
  sensitive   = true
}
