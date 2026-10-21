# Issue #714: a failed purchase or scheduled task must raise an alert that
# reaches the configured email channels.
mock_provider "google" {}

variables {
  project_id            = "example-project"
  service_name          = "cudly"
  environment           = "dev"
  db_instance_id        = "cudly-db"
  service_url           = "cudly.example.com"
  alert_email_addresses = ["alerts-a@example.com", "alerts-b@example.com"]
}

run "failure_markers_raise_alerts" {
  command = apply

  assert {
    condition     = strcontains(google_logging_metric.failure_marker["purchase_failed"].filter, "textPayload:\"purchase_execution_failed\"")
    error_message = "The purchase-failure metric must match the purchase_execution_failed marker in the plain-text log line."
  }

  assert {
    condition     = strcontains(google_logging_metric.failure_marker["scheduled_task_failed"].filter, "textPayload:\"scheduled_task_failed\"")
    error_message = "The scheduled-task metric must match the scheduled_task_failed marker."
  }

  assert {
    condition = alltrue([
      for k, p in google_monitoring_alert_policy.failure_marker :
      strcontains(p.conditions[0].condition_threshold[0].filter, "logging.googleapis.com/user/${google_logging_metric.failure_marker[k].name}") &&
      p.conditions[0].condition_threshold[0].comparison == "COMPARISON_GT" &&
      p.conditions[0].condition_threshold[0].threshold_value == 0
    ])
    error_message = "Each alert must fire on any occurrence of its own metric."
  }

  assert {
    condition = alltrue([
      for p in google_monitoring_alert_policy.failure_marker :
      toset(p.notification_channels) == toset(google_monitoring_notification_channel.email[*].id) &&
      length(p.notification_channels) == 2
    ])
    error_message = "The failure alerts must notify every configured email channel."
  }
}
