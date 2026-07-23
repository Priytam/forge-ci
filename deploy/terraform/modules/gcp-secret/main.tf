# Generates FORGE_SECRET_KEY + runner token and stores them in GCP Secret
# Manager. Values are exported (sensitive) for the forge-release module.

resource "random_bytes" "forge_secret_key" {
  length = 32
}

resource "random_password" "runner_token" {
  count   = var.generate_runner_token ? 1 : 0
  length  = 40
  special = false
}

locals {
  forge_secret_key = random_bytes.forge_secret_key.base64
  runner_token     = var.generate_runner_token ? random_password.runner_token[0].result : ""
}

resource "google_secret_manager_secret" "this" {
  secret_id = var.secret_id
  replication {
    auto {}
  }
}

resource "google_secret_manager_secret_version" "this" {
  secret = google_secret_manager_secret.this.id
  secret_data = jsonencode({
    FORGE_SECRET_KEY = local.forge_secret_key
    RUNNER_TOKEN     = local.runner_token
    WEBHOOK_SECRET   = var.webhook_secret
  })
}
