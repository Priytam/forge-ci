output "release_name" {
  description = "Installed Helm release name."
  value       = helm_release.forge.name
}

output "namespace" {
  description = "Namespace Forge CI is installed in."
  value       = var.namespace
}

output "secret_name" {
  description = "Name of the Kubernetes Secret holding sensitive env."
  value       = kubernetes_secret.forge.metadata[0].name
}

output "app_url" {
  description = "Public application URL (from the ingress host)."
  value       = var.ingress.enabled && var.ingress.host != "" ? format("%s://%s", var.ingress.tls_enabled ? "https" : "http", var.ingress.host) : "(no ingress host configured)"
}
