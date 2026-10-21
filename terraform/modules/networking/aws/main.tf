# AWS VPC Module with IPv6
# Creates VPC with IPv6 support. NAT (fck-nat) handles egress for ECR pulls
# and any AWS APIs that lack IPv6, so we no longer need per-service VPC
# interface endpoints.
# Supports: new VPC, existing VPC, or default VPC

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

# ==============================================
# Data Sources for Existing/Default VPC
# ==============================================

# Get default VPC (if use_default_vpc = true)
data "aws_vpc" "default" {
  count   = var.use_default_vpc ? 1 : 0
  default = true
}

# Get existing VPC (if use_existing_vpc = true and existing_vpc_id provided)
data "aws_vpc" "existing" {
  count = var.use_existing_vpc && var.existing_vpc_id != "" && !var.use_default_vpc ? 1 : 0
  id    = var.existing_vpc_id
}

# Get default subnets (if using default VPC)
data "aws_subnets" "default" {
  count = var.use_default_vpc ? 1 : 0

  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default[0].id]
  }
}

# Local values to determine which VPC/subnets to use
locals {
  create_vpc = !var.use_existing_vpc && !var.use_default_vpc

  vpc_id = local.create_vpc ? aws_vpc.main[0].id : (
    var.use_default_vpc ? data.aws_vpc.default[0].id : data.aws_vpc.existing[0].id
  )

  vpc_cidr = local.create_vpc ? var.vpc_cidr : (
    var.use_default_vpc ? data.aws_vpc.default[0].cidr_block : data.aws_vpc.existing[0].cidr_block
  )

  # IPv6 CIDR (only available if VPC has IPv6 enabled)
  vpc_ipv6_cidr = local.create_vpc ? aws_vpc.main[0].ipv6_cidr_block : (
    var.use_default_vpc ? try(data.aws_vpc.default[0].ipv6_cidr_block, null) : try(data.aws_vpc.existing[0].ipv6_cidr_block, null)
  )

  # Subnet IDs
  public_subnet_ids = local.create_vpc ? aws_subnet.public[*].id : (
    var.use_default_vpc ? data.aws_subnets.default[0].ids : var.existing_public_subnet_ids
  )

  private_subnet_ids = local.create_vpc ? aws_subnet.private[*].id : (
    var.use_default_vpc ? [] : var.existing_private_subnet_ids
  )
}

# ==============================================
# VPC with IPv6 (only created if not using existing VPC)
# ==============================================

resource "aws_vpc" "main" {
  count = local.create_vpc ? 1 : 0

  cidr_block                       = var.vpc_cidr
  enable_dns_hostnames             = true
  enable_dns_support               = true
  assign_generated_ipv6_cidr_block = var.enable_ipv6

  tags = merge(var.tags, {
    Name = "${var.stack_name}-vpc"
  })
}

# ==============================================
# Internet Gateway (for both IPv4 and IPv6)
# ==============================================

resource "aws_internet_gateway" "main" {
  count = local.create_vpc ? 1 : 0

  vpc_id = local.vpc_id

  tags = merge(var.tags, {
    Name = "${var.stack_name}-igw"
  })
}

# ==============================================
# Egress-Only Internet Gateway (for IPv6 outbound)
# ==============================================

resource "aws_egress_only_internet_gateway" "main" {
  count = local.create_vpc && var.enable_ipv6 ? 1 : 0

  vpc_id = local.vpc_id

  tags = merge(var.tags, {
    Name = "${var.stack_name}-eigw"
  })
}

# ==============================================
# fck-nat (cost-effective NAT alternative)
# ==============================================

# Get latest fck-nat AMI (ARM64, free tier eligible)
data "aws_ami" "fck_nat" {
  count = var.enable_nat_gateway ? 1 : 0

  most_recent = true
  owners      = ["568608671756"] # fck-nat official AWS account

  filter {
    name   = "name"
    values = ["fck-nat-al2023-hvm-*-arm64-ebs"]
  }

  filter {
    name   = "state"
    values = ["available"]
  }
}

# Security group for fck-nat instance
resource "aws_security_group" "fck_nat" {
  count = var.enable_nat_gateway ? 1 : 0

  name_prefix = "${var.stack_name}-fck-nat-"
  description = "Security group for fck-nat instance"
  vpc_id      = local.vpc_id

  # Allow all traffic from VPC CIDR
  ingress {
    description = "Allow all from VPC"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = [local.vpc_cidr]
  }

  # Allow all outbound traffic
  egress {
    description = "Allow all outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, {
    Name = "${var.stack_name}-fck-nat-sg"
  })
}

# IAM role for fck-nat instance (for SSM access)
resource "aws_iam_role" "fck_nat" {
  count = var.enable_nat_gateway ? 1 : 0

  name_prefix          = "${var.stack_name}-fck-nat-"
  permissions_boundary = var.permissions_boundary_arn

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action = "sts:AssumeRole"
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
    }]
  })

  tags = merge(var.tags, {
    Name = "${var.stack_name}-fck-nat-role"
  })
}

# Attach SSM managed policy for Session Manager access
resource "aws_iam_role_policy_attachment" "fck_nat_ssm" {
  count = var.enable_nat_gateway ? 1 : 0

  role       = aws_iam_role.fck_nat[0].name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

# Instance profile for fck-nat
resource "aws_iam_instance_profile" "fck_nat" {
  count = var.enable_nat_gateway ? 1 : 0

  name_prefix = "${var.stack_name}-fck-nat-"
  role        = aws_iam_role.fck_nat[0].name

  tags = merge(var.tags, {
    Name = "${var.stack_name}-fck-nat-profile"
  })
}

# Persistent ENI the private routes target (#716). The route tables must not
# name an instance's own primary ENI: the ASG replaces the instance (failed
# health check, AZ event) and the replacement comes up with a new primary ENI,
# so a route pinned to the old one blackholes IPv4 egress until the next apply.
# This ENI outlives every instance. Each one the ASG launches attaches it at
# boot as its second interface (see user_data), so the route target never
# changes. It lives in the ASG's only subnet because an ENI can only attach to
# an instance in its own AZ.
resource "aws_network_interface" "fck_nat" {
  count = var.enable_nat_gateway ? 1 : 0

  description       = "${var.stack_name} fck-nat persistent ENI (private route target)"
  subnet_id         = local.public_subnet_ids[0]
  security_groups   = [aws_security_group.fck_nat[0].id]
  source_dest_check = false # NAT forwards traffic it is neither source nor destination of

  tags = merge(var.tags, {
    Name = "${var.stack_name}-fck-nat-eni"
  })
}

# Launch template for fck-nat instance
resource "aws_launch_template" "fck_nat" {
  count = var.enable_nat_gateway ? 1 : 0

  name_prefix   = "${var.stack_name}-fck-nat-"
  image_id      = data.aws_ami.fck_nat[0].id
  instance_type = "t4g.nano" # ARM64, free tier eligible

  iam_instance_profile {
    name = aws_iam_instance_profile.fck_nat[0].name
  }

  network_interfaces {
    associate_public_ip_address = true
    delete_on_termination       = true
    security_groups             = [aws_security_group.fck_nat[0].id]
  }

  # Hand the persistent ENI to the AMI's own fck-nat service. With eni_id set
  # it disables source/destination checking on the primary ENI, attaches
  # eni_id as the second interface (retrying until a predecessor that is
  # still terminating has released it), and NATs traffic arriving on it out
  # of the primary interface.
  user_data = base64encode(<<-EOF
    #!/bin/sh
    : > /etc/fck-nat.conf
    echo "eni_id=${aws_network_interface.fck_nat[0].id}" >> /etc/fck-nat.conf
    service fck-nat restart
  EOF
  )

  # Disable source/destination check for NAT functionality
  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required" # IMDSv2 required for security
  }

  tag_specifications {
    resource_type = "instance"
    tags = merge(var.tags, {
      Name = "${var.stack_name}-fck-nat"
    })
  }

  tag_specifications {
    resource_type = "volume"
    tags = merge(var.tags, {
      Name = "${var.stack_name}-fck-nat-volume"
    })
  }

  lifecycle {
    create_before_destroy = true
  }
}

# Auto Scaling Group for fck-nat (ensures it stays running)
resource "aws_autoscaling_group" "fck_nat" {
  count = var.enable_nat_gateway ? 1 : 0

  name_prefix         = "${var.stack_name}-fck-nat-"
  desired_capacity    = 1
  min_size            = 1
  max_size            = 1
  vpc_zone_identifier = [aws_network_interface.fck_nat[0].subnet_id] # The persistent ENI's subnet (first public subnet)

  launch_template {
    id      = aws_launch_template.fck_nat[0].id
    version = "$Latest"
  }

  health_check_type         = "EC2"
  health_check_grace_period = 60

  # Grant the attach permission before an instance can boot and ask for it.
  # fck-nat retries the attach, but its one-shot source/dest-check call on
  # the primary ENI does not, so make the order explicit.
  depends_on = [aws_iam_role_policy.fck_nat_ec2]

  tag {
    key                 = "Name"
    value               = "${var.stack_name}-fck-nat-asg"
    propagate_at_launch = false
  }

  dynamic "tag" {
    for_each = var.tags
    content {
      key                 = tag.key
      value               = tag.value
      propagate_at_launch = false
    }
  }

  lifecycle {
    create_before_destroy = true

    # A running instance never re-reads user data, and the launch template
    # version is "$Latest", so a user_data change alone would leave the
    # current instance on the old config. That matters once, on the upgrade
    # that introduces the persistent ENI: the routes would move to an ENI no
    # instance has attached. Replacing the ASG (create-before-destroy, so the
    # new instance is InService before the old one goes) launches one that
    # attaches it. user_data only changes when the ENI itself is replaced, so
    # AMI rotations (image_id) still do not cycle the instance.
    replace_triggered_by = [aws_launch_template.fck_nat[count.index].user_data]
  }
}

# IAM policy for fck-nat to modify its own network interface
resource "aws_iam_role_policy" "fck_nat_ec2" {
  count = var.enable_nat_gateway ? 1 : 0

  name = "ec2-network-interface"
  role = aws_iam_role.fck_nat[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        # Attach the persistent ENI to whichever instance the ASG launched.
        # AttachNetworkInterface is authorized against both the ENI and the
        # instance, so both ARNs are needed; the ENI one is what scopes it.
        Sid      = "AttachPersistentENI"
        Effect   = "Allow"
        Action   = ["ec2:AttachNetworkInterface"]
        Resource = [aws_network_interface.fck_nat[0].arn, "arn:aws:ec2:${var.region}:*:instance/*"]
      },
      {
        # Disable source/destination checking on the instance's own primary
        # ENI, whose ID is only known at boot.
        Sid      = "DisableSourceDestCheck"
        Effect   = "Allow"
        Action   = ["ec2:ModifyNetworkInterfaceAttribute"]
        Resource = "*"
      },
    ]
  })
}

# ==============================================
# Availability Zones
# ==============================================

data "aws_availability_zones" "available" {
  state = "available"

  # Exclude local zones
  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}

# ==============================================
# Public Subnets (for ALB, bastion) - only created for new VPC
# ==============================================

resource "aws_subnet" "public" {
  count = local.create_vpc ? var.az_count : 0

  vpc_id                          = local.vpc_id
  cidr_block                      = cidrsubnet(var.vpc_cidr, 8, count.index)
  ipv6_cidr_block                 = var.enable_ipv6 ? cidrsubnet(local.vpc_ipv6_cidr, 8, count.index) : null
  availability_zone               = data.aws_availability_zones.available.names[count.index]
  map_public_ip_on_launch         = true
  assign_ipv6_address_on_creation = var.enable_ipv6

  tags = merge(var.tags, {
    Name = "${var.stack_name}-public-${data.aws_availability_zones.available.names[count.index]}"
    Type = "public"
  })
}

# ==============================================
# Private Subnets (for Lambda, RDS, Fargate) - only created for new VPC
# ==============================================

resource "aws_subnet" "private" {
  count = local.create_vpc ? var.az_count : 0

  vpc_id                          = local.vpc_id
  cidr_block                      = cidrsubnet(var.vpc_cidr, 8, count.index + var.az_count)
  ipv6_cidr_block                 = var.enable_ipv6 ? cidrsubnet(local.vpc_ipv6_cidr, 8, count.index + var.az_count) : null
  availability_zone               = data.aws_availability_zones.available.names[count.index]
  assign_ipv6_address_on_creation = var.enable_ipv6

  tags = merge(var.tags, {
    Name = "${var.stack_name}-private-${data.aws_availability_zones.available.names[count.index]}"
    Type = "private"
  })

  lifecycle {
    precondition {
      condition     = var.enable_nat_gateway
      error_message = "enable_nat_gateway must be true when creating private subnets: without it, module-created private subnets have no IPv4 path to AWS services (Secrets Manager, ECR, etc.). Enable the fck-nat instance or use an existing VPC with its own egress."
    }
  }
}

# ==============================================
# Route Tables (only for new VPC)
# ==============================================

# Public route table (for both IPv4 and IPv6)
resource "aws_route_table" "public" {
  count = local.create_vpc ? 1 : 0

  vpc_id = local.vpc_id

  tags = merge(var.tags, {
    Name = "${var.stack_name}-public-rt"
    Type = "public"
  })
}

# IPv4 route to Internet Gateway
resource "aws_route" "public_internet_ipv4" {
  count = local.create_vpc ? 1 : 0

  route_table_id         = aws_route_table.public[0].id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.main[0].id
}

# IPv6 route to Internet Gateway
resource "aws_route" "public_internet_ipv6" {
  count = local.create_vpc && var.enable_ipv6 ? 1 : 0

  route_table_id              = aws_route_table.public[0].id
  destination_ipv6_cidr_block = "::/0"
  gateway_id                  = aws_internet_gateway.main[0].id
}

# Associate public subnets with public route table
resource "aws_route_table_association" "public" {
  count = local.create_vpc ? var.az_count : 0

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public[0].id
}

# Private route tables (one per AZ for flexibility)
resource "aws_route_table" "private" {
  count = local.create_vpc ? var.az_count : 0

  vpc_id = local.vpc_id

  tags = merge(var.tags, {
    Name = "${var.stack_name}-private-rt-${count.index + 1}"
    Type = "private"
  })
}

# IPv6 egress route for private subnets
resource "aws_route" "private_internet_ipv6" {
  count = local.create_vpc && var.enable_ipv6 ? var.az_count : 0

  route_table_id              = aws_route_table.private[count.index].id
  destination_ipv6_cidr_block = "::/0"
  egress_only_gateway_id      = aws_egress_only_internet_gateway.main[0].id
}

# IPv4 egress route via fck-nat (optional, for services without IPv6 support like SES)
resource "aws_route" "private_internet_ipv4" {
  count = local.create_vpc && var.enable_nat_gateway ? var.az_count : 0

  route_table_id         = aws_route_table.private[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  network_interface_id   = aws_network_interface.fck_nat[0].id # Persistent, survives instance replacement (#716)

  # Move the route only once an instance that attaches the ENI is InService,
  # so the upgrade that introduces the ENI does not point egress at an
  # unattached interface while the replacement ASG is still launching.
  depends_on = [aws_autoscaling_group.fck_nat]
}

# Associate private subnets with private route tables
resource "aws_route_table_association" "private" {
  count = local.create_vpc ? var.az_count : 0

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[count.index].id
}

# ==============================================
# Security Groups
# ==============================================

# Security group for database access
resource "aws_security_group" "database" {
  name_prefix = "${var.stack_name}-database-"
  description = "Security group for database access"
  vpc_id      = local.vpc_id

  # PostgreSQL from VPC (IPv4)
  ingress {
    description = "PostgreSQL from VPC (IPv4)"
    from_port   = 5432
    to_port     = 5432
    protocol    = "tcp"
    cidr_blocks = [local.vpc_cidr]
  }

  # PostgreSQL from VPC (IPv6)
  ingress {
    description      = "PostgreSQL from VPC (IPv6)"
    from_port        = 5432
    to_port          = 5432
    protocol         = "tcp"
    ipv6_cidr_blocks = var.enable_ipv6 ? [local.vpc_ipv6_cidr] : []
  }

  # Allow all outbound (IPv4)
  egress {
    description = "Allow all outbound (IPv4)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  # Allow all outbound (IPv6)
  egress {
    description      = "Allow all outbound (IPv6)"
    from_port        = 0
    to_port          = 0
    protocol         = "-1"
    ipv6_cidr_blocks = ["::/0"]
  }

  tags = merge(var.tags, {
    Name = "${var.stack_name}-database-sg"
  })

  lifecycle {
    create_before_destroy = true
  }
}

# Security group for ALB (if using Fargate)
resource "aws_security_group" "alb" {
  count = var.create_alb_security_group ? 1 : 0

  name_prefix = "${var.stack_name}-alb-"
  description = "Security group for Application Load Balancer"
  vpc_id      = local.vpc_id

  # HTTP from internet (IPv4)
  ingress {
    description = "HTTP from internet (IPv4)"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  # HTTP from internet (IPv6)
  ingress {
    description      = "HTTP from internet (IPv6)"
    from_port        = 80
    to_port          = 80
    protocol         = "tcp"
    ipv6_cidr_blocks = ["::/0"]
  }

  # HTTPS from internet (IPv4)
  ingress {
    description = "HTTPS from internet (IPv4)"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  # HTTPS from internet (IPv6)
  ingress {
    description      = "HTTPS from internet (IPv6)"
    from_port        = 443
    to_port          = 443
    protocol         = "tcp"
    ipv6_cidr_blocks = ["::/0"]
  }

  # Allow all outbound (IPv4)
  egress {
    description = "Allow all outbound (IPv4)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  # Allow all outbound (IPv6)
  egress {
    description      = "Allow all outbound (IPv6)"
    from_port        = 0
    to_port          = 0
    protocol         = "-1"
    ipv6_cidr_blocks = ["::/0"]
  }

  tags = merge(var.tags, {
    Name = "${var.stack_name}-alb-sg"
  })

  lifecycle {
    create_before_destroy = true
  }
}

# ==============================================
# VPC Flow Logs (optional, for debugging)
# ==============================================

resource "aws_flow_log" "main" {
  count = var.enable_flow_logs ? 1 : 0

  iam_role_arn    = aws_iam_role.flow_logs[0].arn
  log_destination = aws_cloudwatch_log_group.flow_logs[0].arn
  traffic_type    = "ALL"
  vpc_id          = local.vpc_id

  tags = merge(var.tags, {
    Name = "${var.stack_name}-flow-logs"
  })
}

resource "aws_cloudwatch_log_group" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  name              = "/aws/vpc/${var.stack_name}"
  retention_in_days = var.flow_logs_retention_days

  tags = var.tags
}

resource "aws_iam_role" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  name_prefix          = "${var.stack_name}-flow-logs-"
  permissions_boundary = var.permissions_boundary_arn

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Action = "sts:AssumeRole"
        Effect = "Allow"
        Principal = {
          Service = "vpc-flow-logs.amazonaws.com"
        }
      }
    ]
  })

  tags = var.tags
}

resource "aws_iam_role_policy" "flow_logs" {
  count = var.enable_flow_logs ? 1 : 0

  name_prefix = "${var.stack_name}-flow-logs-"
  role        = aws_iam_role.flow_logs[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Action = [
          "logs:CreateLogGroup",
          "logs:CreateLogStream",
          "logs:PutLogEvents",
          "logs:DescribeLogGroups",
          "logs:DescribeLogStreams"
        ]
        Effect   = "Allow"
        Resource = "*"
      }
    ]
  })
}
