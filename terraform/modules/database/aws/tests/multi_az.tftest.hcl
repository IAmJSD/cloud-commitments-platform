# Multi-AZ support (#719). The module could not express a standby replica, so
# prod on AWS had no HA while GCP (REGIONAL) and Azure (ZoneRedundant) did.
# The environment wiring is pinned by TestAWSDatabaseMultiAZIsWired in
# terraform/environments/aws.
mock_provider "aws" {}
mock_provider "random" {}

variables {
  stack_name               = "cudly-test"
  permissions_boundary_arn = "arn:aws:iam::123456789012:policy/cudly-boundary"
  vpc_id                   = "vpc-0123456789abcdef0"
  vpc_cidr                 = "10.0.0.0/16"
  private_subnet_ids       = ["subnet-0aaaaaaaaaaaaaaaa", "subnet-0bbbbbbbbbbbbbbbb"]
  admin_email              = "admin@example.com"
}

run "single_az_by_default" {
  command = plan

  assert {
    condition     = aws_db_instance.main.multi_az == false
    error_message = "multi_az must default to false so dev/staging keep their single-AZ cost profile."
  }
}

run "multi_az_reaches_the_instance" {
  command = plan

  variables {
    multi_az = true
  }

  assert {
    condition     = aws_db_instance.main.multi_az == true
    error_message = "multi_az = true must provision a Multi-AZ standby on aws_db_instance.main."
  }
}
