output "forge_secret_key" {
  description = "base64 32-byte AES-256-GCM key."
  value       = local.forge_secret_key
  sensitive   = true
}

output "runner_token" {
  value     = local.runner_token
  sensitive = true
}

output "secret_arn" {
  value = aws_secretsmanager_secret.this.arn
}
