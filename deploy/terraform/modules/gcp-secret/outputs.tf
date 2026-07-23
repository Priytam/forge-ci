output "forge_secret_key" {
  value     = local.forge_secret_key
  sensitive = true
}

output "runner_token" {
  value     = local.runner_token
  sensitive = true
}

output "secret_id" {
  value = google_secret_manager_secret.this.secret_id
}
