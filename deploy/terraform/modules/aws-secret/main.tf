# Generates FORGE_SECRET_KEY (base64 32 bytes) and a runner token, and stores
# them in AWS Secrets Manager so they survive independently of Helm/TF state.
# The values are also exported (sensitive) so the forge-release module can wire
# them into the in-cluster Secret.

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

resource "aws_secretsmanager_secret" "this" {
  name                    = var.secret_name
  description             = "Forge CI sensitive env (FORGE_SECRET_KEY, RUNNER_TOKEN, WEBHOOK_SECRET)."
  recovery_window_in_days = var.recovery_window_in_days
  tags                    = var.tags
}

resource "aws_secretsmanager_secret_version" "this" {
  secret_id = aws_secretsmanager_secret.this.id
  secret_string = jsonencode({
    FORGE_SECRET_KEY = local.forge_secret_key
    RUNNER_TOKEN     = local.runner_token
    WEBHOOK_SECRET   = var.webhook_secret
  })
}
