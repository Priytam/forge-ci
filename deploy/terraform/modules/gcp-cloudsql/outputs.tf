output "private_ip_address" {
  value = google_sql_database_instance.this.private_ip_address
}

output "connection_name" {
  value = google_sql_database_instance.this.connection_name
}

output "database_url" {
  description = "DSN wired into the Helm release (DATABASE_URL), via private IP."
  value       = format("postgres://%s:%s@%s:5432/%s?sslmode=%s", var.username, local.password, google_sql_database_instance.this.private_ip_address, var.db_name, var.sslmode)
  sensitive   = true
}
