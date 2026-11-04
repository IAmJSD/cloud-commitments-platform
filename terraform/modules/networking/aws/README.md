# AWS Networking Module

This module supports three VPC deployment modes:

## 1. Create New VPC (Default)

Creates a new VPC with IPv6 support, public and private subnets, and optional fck-nat for IPv4 egress.

```hcl
module "networking" {
  source = "./modules/networking/aws"

  stack_name  = "myapp"
  environment = "dev"
  region      = "us-east-1"

  vpc_cidr = "10.0.0.0/16"
  az_count = 2

  enable_nat_gateway = true  # Optional: fck-nat for IPv4 egress

  tags = {
    Project = "MyApp"
  }
}
```

**Features:**

- New VPC with IPv6 support
- Public and private subnets across multiple AZs
- Internet Gateway and Egress-Only Internet Gateway
- Optional fck-nat instance (~$3/month) for IPv4 egress
- VPC Endpoints for Secrets Manager

## 2. Use Existing VPC

Use an existing VPC with your own subnets.

```hcl
module "networking" {
  source = "./modules/networking/aws"

  stack_name  = "myapp"
  environment = "dev"
  region      = "us-east-1"

  use_existing_vpc = true
  existing_vpc_id  = "vpc-12345678"

  existing_public_subnet_ids  = ["subnet-11111111", "subnet-22222222"]
  existing_private_subnet_ids = ["subnet-33333333", "subnet-44444444"]

  # Optional: Add fck-nat for IPv4 egress
  enable_nat_gateway = true

  tags = {
    Project = "MyApp"
  }
}
```

**Features:**

- Uses your existing VPC
- Requires you to specify subnet IDs
- Can optionally add fck-nat for IPv4 egress (will be placed in first public subnet)
- Creates security groups in your VPC
- Can add VPC Endpoints

## 3. Use Default VPC

Use the AWS default VPC in the region.

```hcl
module "networking" {
  source = "./modules/networking/aws"

  stack_name  = "myapp"
  environment = "dev"
  region      = "us-east-1"

  use_default_vpc = true

  # Optional: Add fck-nat for IPv4 egress
  enable_nat_gateway = true

  tags = {
    Project = "MyApp"
  }
}
```

**Features:**

- Automatically discovers and uses the default VPC
- Uses all default subnets (typically all public)
- Can optionally add fck-nat for IPv4 egress
- Creates security groups in the default VPC
- Minimal configuration required

## IPv6 Support

When creating a new VPC, IPv6 is automatically enabled. For existing VPCs:

- If the VPC has IPv6 enabled, the module will use it
- If not, IPv4-only configuration will be used

## fck-nat Instance

The `enable_nat_gateway` option deploys a cost-effective NAT alternative:

- t4g.nano ARM64 instance (~$3/month)
- Provides IPv4 egress for services without IPv6 support (like AWS SES)
- Automatically deployed in the first public subnet
- 90% cheaper than AWS NAT Gateway (~$32/month)
- Private IPv4 routes target a persistent ENI that each instance attaches at
  boot, so egress survives the ASG replacing the instance. With an existing
  VPC, route private subnets to the `nat_network_interface_id` output.

### Rollback runbook (persistent ENI, #716)

If egress from the private subnets is down after the apply that introduced the
persistent ENI, run `rollback.yml` against the pre-merge `main` SHA first. Note
that `rollback.yml` applies the environment root from that ref's checkout.

Fallback by hand, when the workflow cannot run. Placeholders: `<eni-id>` is the
`nat_network_interface_id` output, `<instance-id>` the running fck-nat instance
(`nat_autoscaling_group_name` output, then `aws autoscaling describe-auto-scaling-groups`),
`<rtb-id>` each private route table.

```bash
# 1. Is the ENI attached as the instance's second interface?
aws ec2 describe-network-interfaces --network-interface-ids <eni-id> \
  --query 'NetworkInterfaces[0].Attachment'

# 2. If not, attach it by hand (device index 1; the instance must be in the ENI's AZ).
aws ec2 attach-network-interface --network-interface-id <eni-id> \
  --instance-id <instance-id> --device-index 1

# 3. If the route is a blackhole, point it back at a working target
#    (the instance's primary ENI, or the ENI once step 2 succeeded).
aws ec2 replace-route --route-table-id <rtb-id> \
  --destination-cidr-block 0.0.0.0/0 --network-interface-id <eni-id>
```

A persistent attach failure shows as an instance replaced about every 5 minutes
(`aws autoscaling describe-scaling-activities --auto-scaling-group-name <asg-name>`):
user data bounds the service restart at 300 s and powers the instance off, and
the launch template's `terminate` shutdown behaviour has the ASG replace it.

## Security Groups

The module creates:

- **Database Security Group**: PostgreSQL access from VPC CIDR
- **VPC Endpoints Security Group**: HTTPS access for VPC endpoints
- **ALB Security Group** (optional): HTTP/HTTPS from internet, or, with
  `restrict_alb_ingress_to_cloudfront = true`, HTTP from the CloudFront
  origin-facing managed prefix list only
- **fck-nat Security Group** (if enabled): NAT traffic from VPC

## Outputs

The module outputs:

- `vpc_id`: VPC ID
- `vpc_cidr`: VPC CIDR block (IPv4)
- `vpc_ipv6_cidr`: VPC CIDR block (IPv6, if available)
- `public_subnet_ids`: List of public subnet IDs
- `private_subnet_ids`: List of private subnet IDs
- `database_security_group_id`: Security group for database
- `lambda_vpc_config`: Convenience object for Lambda module
- `database_vpc_config`: Convenience object for database module
- `nat_network_interface_id`: Persistent fck-nat ENI to route IPv4 egress to (if enabled)
- `nat_autoscaling_group_name`: fck-nat Auto Scaling group name (if enabled)

## Cost Comparison

| Mode | Monthly Cost | Notes |
|------|--------------|-------|
| New VPC (IPv6 only) | ~$0 | Free, but limited to IPv6 services |
| New VPC + fck-nat | ~$3 | IPv4 + IPv6 egress |
| Existing/Default VPC | ~$0 | Uses your existing infrastructure |
| Existing VPC + fck-nat | ~$3 | Adds IPv4 NAT to existing VPC |

Compare to AWS NAT Gateway: ~$32/month + data transfer costs

## Examples

### Development Environment (Default VPC)

```hcl
use_default_vpc    = true
enable_nat_gateway = true  # For email sending via SES
```

### Production (New VPC with High Availability)

```hcl
vpc_cidr           = "10.0.0.0/16"
az_count           = 3
enable_nat_gateway = true
enable_flow_logs   = true
```

### Use Existing Corporate VPC

```hcl
use_existing_vpc            = true
existing_vpc_id             = "vpc-corporate"
existing_public_subnet_ids  = ["subnet-pub-1", "subnet-pub-2"]
existing_private_subnet_ids = ["subnet-priv-1", "subnet-priv-2"]
```
