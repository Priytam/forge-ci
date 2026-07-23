output "vpc_id" {
  description = "VPC id."
  value       = aws_vpc.this.id
}

output "public_subnet_ids" {
  description = "Public subnet ids."
  value       = aws_subnet.public[*].id
}

output "private_subnet_ids" {
  description = "Private subnet ids (EKS nodes + RDS)."
  value       = aws_subnet.private[*].id
}
