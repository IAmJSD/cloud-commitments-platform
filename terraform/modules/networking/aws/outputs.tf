output "vpc_id" {
  description = "VPC ID"
  value       = local.vpc_id
}

output "vpc_cidr" {
  description = "VPC CIDR block (IPv4)"
  value       = local.vpc_cidr
}

output "vpc_ipv6_cidr" {
  description = "VPC CIDR block (IPv6)"
  value       = local.vpc_ipv6_cidr
}

output "public_subnet_ids" {
  description = "List of public subnet IDs"
  value       = local.public_subnet_ids
}

output "private_subnet_ids" {
  description = "List of private subnet IDs"
  value       = local.private_subnet_ids
}

output "public_subnet_cidrs" {
  description = "List of public subnet CIDR blocks (IPv4)"
  value       = local.create_vpc ? aws_subnet.public[*].cidr_block : []
}

output "private_subnet_cidrs" {
  description = "List of private subnet CIDR blocks (IPv4)"
  value       = local.create_vpc ? aws_subnet.private[*].cidr_block : []
}

output "public_subnet_ipv6_cidrs" {
  description = "List of public subnet CIDR blocks (IPv6)"
  value       = local.create_vpc ? aws_subnet.public[*].ipv6_cidr_block : []
}

output "private_subnet_ipv6_cidrs" {
  description = "List of private subnet CIDR blocks (IPv6)"
  value       = local.create_vpc ? aws_subnet.private[*].ipv6_cidr_block : []
}

output "availability_zones" {
  description = "List of availability zones used"
  value       = local.create_vpc ? aws_subnet.private[*].availability_zone : []
}

output "database_security_group_id" {
  description = "Security group ID for database access"
  value       = aws_security_group.database.id
}

output "alb_security_group_id" {
  description = "Security group ID for ALB (if created)"
  value       = var.create_alb_security_group ? aws_security_group.alb[0].id : null
}

# Derived from the security group's inline ingress rules, not just from
# var.restrict_alb_ingress_to_cloudfront, so an inline rule added back to the
# group that admits anything other than the CloudFront origin-facing prefix
# list turns this false (and trips the environment's #412 check block). Rules
# attached from outside this resource (aws_vpc_security_group_ingress_rule)
# are not visible here.
output "alb_ingress_cloudfront_only" {
  description = "True when every ingress rule on the ALB security group admits only the CloudFront origin-facing managed prefix list. False when the group is not created."
  value = var.create_alb_security_group && length(data.aws_ec2_managed_prefix_list.cloudfront_origin_facing) > 0 ? alltrue([
    for r in aws_security_group.alb[0].ingress :
    length(r.cidr_blocks == null ? [] : r.cidr_blocks) == 0 &&
    length(r.ipv6_cidr_blocks == null ? [] : r.ipv6_cidr_blocks) == 0 &&
    length(r.security_groups == null ? [] : r.security_groups) == 0 &&
    !(r.self == true) &&
    toset(r.prefix_list_ids == null ? [] : r.prefix_list_ids) == toset([data.aws_ec2_managed_prefix_list.cloudfront_origin_facing[0].id])
  ]) : false
}

output "internet_gateway_id" {
  description = "Internet Gateway ID"
  value       = local.create_vpc ? aws_internet_gateway.main[0].id : null
}

output "egress_only_gateway_id" {
  description = "Egress-Only Internet Gateway ID (IPv6)"
  value       = local.create_vpc && var.enable_ipv6 ? aws_egress_only_internet_gateway.main[0].id : null
}

output "public_route_table_id" {
  description = "Public route table ID"
  value       = local.create_vpc ? aws_route_table.public[0].id : null
}

output "private_route_table_ids" {
  description = "List of private route table IDs"
  value       = local.create_vpc ? aws_route_table.private[*].id : []
}

# Convenience output for Lambda module
output "lambda_vpc_config" {
  description = "VPC configuration object for Lambda module"
  value = {
    vpc_id                        = local.vpc_id
    subnet_ids                    = local.private_subnet_ids
    additional_security_group_ids = []
  }
}

# Convenience output for database module
output "database_vpc_config" {
  description = "VPC configuration object for database module"
  value = {
    vpc_id                = local.vpc_id
    subnet_ids            = local.private_subnet_ids
    database_subnet_group = null # Will be created by database module
    security_group_id     = aws_security_group.database.id
  }
}

# The persistent fck-nat ENI (#716). The module routes its own private route
# tables to it; with use_existing_vpc, point the VPC's private 0.0.0.0/0
# routes here rather than at the fck-nat instance, whose primary ENI changes
# every time the ASG replaces it.
output "nat_network_interface_id" {
  description = "ID of the persistent fck-nat ENI to route private IPv4 egress to (null when enable_nat_gateway is false)"
  value       = var.enable_nat_gateway ? aws_network_interface.fck_nat[0].id : null
}

# The fck-nat ASG, for alarms on its instance replacements. It is replaced
# (new name) when the launch template's user data changes; see the ASG's
# replace_triggered_by.
output "nat_autoscaling_group_name" {
  description = "Name of the fck-nat Auto Scaling group (null when enable_nat_gateway is false)"
  value       = var.enable_nat_gateway ? aws_autoscaling_group.fck_nat[0].name : null
}
