output "endpoint" {
  description = "RDS endpoint host:port."
  value       = aws_db_instance.this.endpoint
}

output "address" {
  description = "RDS hostname."
  value       = aws_db_instance.this.address
}

output "database_url" {
  description = "Full DSN wired into the Helm release (DATABASE_URL)."
  value       = format("postgres://%s:%s@%s:5432/%s?sslmode=%s", var.username, local.password, aws_db_instance.this.address, var.db_name, var.sslmode)
  sensitive   = true
}
