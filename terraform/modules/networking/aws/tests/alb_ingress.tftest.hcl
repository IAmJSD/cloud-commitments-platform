# ALB ingress restriction (#412). With CloudFront in front of the Fargate ALB
# (restrict_alb_ingress_to_cloudfront = true), the ALB security group must
# admit only CloudFront's origin-facing managed prefix list, so clients cannot
# bypass the distribution by calling the ALB directly. With it off, the group
# keeps exactly the four internet-facing rules it had before #412.
mock_provider "aws" {
  mock_data "aws_ec2_managed_prefix_list" {
    defaults = {
      id = "pl-3b927c52"
    }
  }

  mock_data "aws_availability_zones" {
    defaults = {
      names = ["us-east-1a", "us-east-1b", "us-east-1c"]
    }
  }
}

variables {
  stack_name                = "cudly-test"
  permissions_boundary_arn  = "arn:aws:iam::123456789012:policy/cudly-boundary"
  region                    = "us-east-1"
  create_alb_security_group = true
  enable_nat_gateway        = true
}

run "default_keeps_internet_facing_rules" {
  command = plan

  assert {
    condition = toset([for r in aws_security_group.alb[0].ingress : r.description]) == toset([
      "HTTP from internet (IPv4)",
      "HTTP from internet (IPv6)",
      "HTTPS from internet (IPv4)",
      "HTTPS from internet (IPv6)",
    ])
    error_message = "With the restriction off, the ALB group must keep its four pre-#412 internet-facing rules."
  }

  assert {
    condition = alltrue([
      for r in aws_security_group.alb[0].ingress :
      r.protocol == "tcp" && r.from_port == r.to_port && contains([80, 443], r.from_port) &&
      (contains(coalesce(r.cidr_blocks, ["unset"]), "0.0.0.0/0") || contains(coalesce(r.ipv6_cidr_blocks, ["unset"]), "::/0"))
    ])
    error_message = "With the restriction off, each ALB rule must be tcp 80 or 443 from 0.0.0.0/0 or ::/0."
  }

  assert {
    condition     = length(data.aws_ec2_managed_prefix_list.cloudfront_origin_facing) == 0
    error_message = "The CloudFront prefix list must not be looked up when the restriction is off."
  }

  assert {
    condition     = output.alb_ingress_cloudfront_only == false
    error_message = "alb_ingress_cloudfront_only must be false while the ALB is open to the internet."
  }
}

run "restricted_admits_only_cloudfront_prefix_list" {
  command = plan

  variables {
    restrict_alb_ingress_to_cloudfront = true
  }

  assert {
    condition     = data.aws_ec2_managed_prefix_list.cloudfront_origin_facing[0].name == "com.amazonaws.global.cloudfront.origin-facing"
    error_message = "The restriction must use AWS's CloudFront origin-facing managed prefix list."
  }

  assert {
    condition     = length(aws_security_group.alb[0].ingress) == 1
    error_message = "With the restriction on, the ALB group must have exactly one ingress rule (a prefix-list rule's weight leaves no room for a second)."
  }

  assert {
    condition = alltrue([
      for r in aws_security_group.alb[0].ingress :
      r.protocol == "tcp" && r.from_port == 80 && r.to_port == 80 &&
      toset(r.prefix_list_ids) == toset(["pl-3b927c52"]) &&
      length(coalesce(r.cidr_blocks, [])) == 0 &&
      length(coalesce(r.ipv6_cidr_blocks, [])) == 0
    ])
    error_message = "With the restriction on, the only ALB ingress must be tcp/80 from the CloudFront prefix list, with no CIDR sources."
  }

  assert {
    condition     = output.alb_ingress_cloudfront_only == true
    error_message = "alb_ingress_cloudfront_only must be true when the only ingress is the CloudFront prefix list."
  }

  # Egress is unchanged by the restriction.
  assert {
    condition     = length(aws_security_group.alb[0].egress) == 2
    error_message = "The restriction must not change the ALB group's egress rules."
  }
}

run "restriction_is_inert_without_alb_group" {
  command = plan

  variables {
    create_alb_security_group          = false
    restrict_alb_ingress_to_cloudfront = true
  }

  assert {
    condition     = length(aws_security_group.alb) == 0 && length(data.aws_ec2_managed_prefix_list.cloudfront_origin_facing) == 0
    error_message = "Without an ALB group (Lambda), the restriction must create nothing and look nothing up."
  }

  assert {
    condition     = output.alb_ingress_cloudfront_only == false
    error_message = "alb_ingress_cloudfront_only must be false when no ALB group exists."
  }
}
