# ==============================================
# Database
# ==============================================

locals {
  # Connections kept out of the app's budget: Postgres
  # superuser_reserved_connections (3), the rdsadmin session, and an operator
  # or migration session (#718).
  db_reserved_connections = 10

  db_pool_env = {
    DB_MAX_CONNECTIONS = tostring(var.db_max_connections)
    DB_MIN_CONNECTIONS = tostring(var.db_min_connections)
  }
}

module "database" {
  source = "../../modules/database/aws"

  stack_name               = local.stack_name
  permissions_boundary_arn = local.permissions_boundary_arn

  # Database configuration
  engine_version             = var.database_engine_version
  database_name              = var.database_name
  master_username            = var.database_username
  master_password_secret_arn = module.secrets.database_password_secret_arn # Use secrets module password
  create_password            = false                                       # Password managed by secrets module

  # Instance sizing
  instance_class        = var.database_instance_class
  allocated_storage     = var.database_allocated_storage
  max_allocated_storage = var.database_max_allocated_storage

  # High availability
  multi_az = var.database_multi_az

  # Networking
  vpc_id             = module.networking.vpc_id
  vpc_cidr           = var.vpc_cidr
  private_subnet_ids = module.networking.private_subnet_ids

  # RDS Proxy (only needed for high-concurrency Lambda workloads)
  enable_rds_proxy = false

  # Backups
  backup_retention_days = var.database_backup_retention_days

  # Monitoring
  performance_insights_enabled = var.database_performance_insights

  # Protection
  deletion_protection = var.database_deletion_protection
  skip_final_snapshot = var.database_skip_final_snapshot

  # Admin user configuration
  admin_email = var.admin_email

  tags = local.common_tags

  depends_on = [module.networking, module.secrets]
}
