# Managed Postgres (RDS) for the Forge control plane. Placed in private subnets
# with a security group that only admits the cluster nodes' VPC CIDR.

resource "random_password" "db" {
  count            = var.password == "" ? 1 : 0
  length           = 24
  special          = true
  override_special = "!#$%&*()-_=+[]{}"
}

locals {
  password = var.password != "" ? var.password : random_password.db[0].result
}

resource "aws_db_subnet_group" "this" {
  name       = "${var.name}-db"
  subnet_ids = var.subnet_ids
  tags       = var.tags
}

resource "aws_security_group" "this" {
  name        = "${var.name}-db"
  description = "Forge CI Postgres access"
  vpc_id      = var.vpc_id
  tags        = var.tags

  ingress {
    description = "Postgres from within the VPC"
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = var.allowed_cidr_blocks
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_db_instance" "this" {
  identifier     = "${var.name}-postgres"
  engine         = "postgres"
  engine_version = var.engine_version
  instance_class = var.instance_class

  allocated_storage     = var.allocated_storage
  max_allocated_storage = var.max_allocated_storage
  storage_type          = "gp3"
  storage_encrypted     = true

  db_name  = var.db_name
  username = var.username
  password = local.password
  port     = 5432

  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = [aws_security_group.this.id]

  multi_az                = var.multi_az
  publicly_accessible     = false
  backup_retention_period = var.backup_retention_period
  deletion_protection     = var.deletion_protection
  skip_final_snapshot     = var.skip_final_snapshot
  apply_immediately       = true

  tags = var.tags
}
