# fck-nat egress route survives instance replacement (#716).
#
# The private 0.0.0.0/0 routes used to point at the primary ENI of whichever
# fck-nat instance was running when Terraform last read it. The ASG replaces
# that instance on a failed health check or AZ event, the new instance gets a
# new primary ENI, and the route keeps naming the old one until the next
# apply: private-subnet IPv4 egress is blackholed in the meantime. The routes
# must instead target a persistent ENI that Terraform owns and every instance
# the ASG launches attaches at boot, so the route target never changes when
# the instance does.
mock_provider "aws" {
  # The AZ list and the VPC's IPv6 block feed cidrsubnet() and indexing, so
  # the mock's random defaults cannot stand in for them.
  override_data {
    target = data.aws_availability_zones.available
    values = {
      names = ["us-east-1a", "us-east-1b", "us-east-1c"]
    }
  }

  override_resource {
    target = aws_vpc.main
    values = {
      ipv6_cidr_block = "2600:1f18:1234:5600::/56"
    }
  }

  # The provider validates the ASG's launch_template.id format.
  override_resource {
    target = aws_launch_template.fck_nat
    values = {
      id = "lt-0mock"
    }
  }
}

variables {
  stack_name               = "cudly-test"
  permissions_boundary_arn = "arn:aws:iam::123456789012:policy/cudly-deploy-boundary"
  region                   = "us-east-1"
  az_count                 = 3
  enable_nat_gateway       = true
}

# Not covered here: the routes' depends_on on the ASG, which keeps the
# one-time upgrade from moving egress to the new ENI before an instance that
# attaches it is InService, and the ASG's depends_on on the instance role
# policy. Mock providers expose no apply ordering, so deleting either one
# leaves every run below green; check them by hand when editing main.tf.
run "routes_target_persistent_eni" {
  command = apply

  assert {
    condition     = length(aws_route.private_internet_ipv4) == 3
    error_message = "Every private route table needs its IPv4 default route via fck-nat."
  }

  assert {
    condition = alltrue([
      for r in aws_route.private_internet_ipv4 :
      r.network_interface_id == aws_network_interface.fck_nat[0].id
    ])
    error_message = "Private IPv4 default routes must target the persistent fck-nat ENI, not the primary ENI of the instance running at apply time: the ASG replaces that instance and its ENI with it, blackholing egress (#716)."
  }

  assert {
    condition     = output.nat_network_interface_id == aws_network_interface.fck_nat[0].id
    error_message = "nat_network_interface_id must expose the persistent ENI so existing-VPC route tables can target it too."
  }
}

run "persistent_eni_is_attachable_by_every_instance" {
  command = apply

  # NAT forwards traffic it is neither source nor destination of.
  assert {
    condition     = aws_network_interface.fck_nat[0].source_dest_check == false
    error_message = "The persistent ENI must have source/destination checking disabled or it drops forwarded traffic."
  }

  # An ENI can only attach to an instance in its own AZ.
  assert {
    condition     = toset(aws_autoscaling_group.fck_nat[0].vpc_zone_identifier) == toset([aws_network_interface.fck_nat[0].subnet_id])
    error_message = "The ASG must launch fck-nat only into the persistent ENI's subnet, or a replacement instance in another AZ cannot attach it."
  }

  # The fck-nat AMI's own service attaches eni_id at boot, retrying until a
  # terminating predecessor has released it.
  assert {
    condition     = strcontains(base64decode(aws_launch_template.fck_nat[0].user_data), "eni_id=${aws_network_interface.fck_nat[0].id}")
    error_message = "Launch template user data must hand the persistent ENI to the fck-nat service (eni_id in /etc/fck-nat.conf) so each replacement instance attaches it."
  }

  assert {
    condition     = strcontains(base64decode(aws_launch_template.fck_nat[0].user_data), "service fck-nat restart")
    error_message = "User data must restart the fck-nat service after writing its config, or the ENI is never attached."
  }

  assert {
    condition = anytrue([
      for s in jsondecode(aws_iam_role_policy.fck_nat_ec2[0].policy).Statement :
      contains(flatten([s.Action]), "ec2:AttachNetworkInterface") && contains(flatten([s.Resource]), aws_network_interface.fck_nat[0].arn)
    ])
    error_message = "The fck-nat instance role must be allowed ec2:AttachNetworkInterface on the persistent ENI."
  }
}

# A running instance never re-reads user data, so when the ENI it must attach
# changes (the upgrade that introduces it, or the ENI being replaced later),
# the ASG itself has to be replaced to launch an instance that attaches the
# new one. Mock providers do not reproduce the real provider's ForceNew
# planning, so the ENI replacement is requested explicitly here.
run "eni_replacement_replaces_asg" {
  command = apply

  plan_options {
    replace = [aws_network_interface.fck_nat[0]]
  }

  assert {
    condition     = output.nat_network_interface_id != run.persistent_eni_is_attachable_by_every_instance.nat_network_interface_id
    error_message = "Test precondition: the persistent ENI should have been replaced."
  }

  assert {
    condition     = output.nat_autoscaling_group_name != run.persistent_eni_is_attachable_by_every_instance.nat_autoscaling_group_name
    error_message = "The fck-nat ASG must be replaced when its user data names a new ENI; updated in place, the running instance keeps the old config and never attaches the ENI the routes now target."
  }
}

# The fck-nat AMI lookup is most_recent, so an upstream AMI release changes the
# launch template's image_id on the next apply with nothing else changing.
# That must not cycle the NAT instance.
run "ami_rotation_keeps_asg" {
  command = apply

  override_data {
    target = data.aws_ami.fck_nat
    values = {
      id = "ami-0rotated"
    }
  }

  assert {
    condition     = aws_launch_template.fck_nat[0].image_id == "ami-0rotated"
    error_message = "Test precondition: the launch template should have picked up the rotated AMI."
  }

  assert {
    condition     = output.nat_autoscaling_group_name == run.eni_replacement_replaces_asg.nat_autoscaling_group_name
    error_message = "An AMI rotation alone must not replace the fck-nat ASG (and its running instance)."
  }
}

run "no_nat_no_eni" {
  command = plan

  variables {
    use_existing_vpc            = true
    existing_vpc_id             = "vpc-0existing"
    existing_public_subnet_ids  = ["subnet-0public"]
    existing_private_subnet_ids = ["subnet-0private"]
    enable_nat_gateway          = false
  }

  override_data {
    target = data.aws_vpc.existing
    values = {
      cidr_block      = "10.20.0.0/16"
      ipv6_cidr_block = "2600:1f18:abcd:ef00::/56"
    }
  }

  assert {
    condition     = length(aws_network_interface.fck_nat) == 0 && output.nat_network_interface_id == null
    error_message = "No fck-nat ENI should exist when enable_nat_gateway is false."
  }
}
