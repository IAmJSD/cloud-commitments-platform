# The GCP database module must default to a protected instance, like the AWS
# and Azure modules (#293). A caller that omits deletion_protection used to get
# an instance `terraform destroy` removes without objection.
mock_provider "google" {}
mock_provider "random" {}

variables {
  project_id     = "example-project"
  service_name   = "cudly-test"
  environment    = "prod"
  region         = "us-central1"
  vpc_network_id = "projects/example-project/global/networks/cudly-test"
}

run "protected_by_default" {
  command = plan

  assert {
    condition     = google_sql_database_instance.main.deletion_protection == true
    error_message = "A caller that omits deletion_protection must get a protected Cloud SQL instance, matching the AWS module's default."
  }
}

run "opt_out_is_honored" {
  command = plan

  variables {
    environment         = "dev"
    deletion_protection = false
  }

  assert {
    condition     = google_sql_database_instance.main.deletion_protection == false
    error_message = "deletion_protection = false must reach the instance so dev environments can be destroyed."
  }
}
