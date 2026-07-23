output "network_id" {
  value = google_compute_network.this.id
}

output "network_name" {
  value = google_compute_network.this.name
}

output "subnet_name" {
  value = google_compute_subnetwork.this.name
}

output "pods_range_name" {
  value = "pods"
}

output "services_range_name" {
  value = "services"
}

output "private_vpc_connection" {
  description = "Service networking connection (depend on this for Cloud SQL private IP)."
  value       = google_service_networking_connection.psa.id
}
