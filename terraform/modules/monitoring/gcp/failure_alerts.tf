# ==============================================
# Failure alerts (issue #714)
# ==============================================
#
# The application logs a stable marker token for the two failures operators
# must hear about; each becomes a log-based metric plus an alert policy that
# fires on any occurrence:
#
#   purchase_execution_failed  an execution row ended failed or
#                              partially_completed
#                              (internal/purchase/failure_notify.go)
#   scheduled_task_failed      a scheduled task returned an error, including a
#                              process_scheduled_purchases run with failures
#                              (internal/server/scheduled_tasks.go)
#
# The Go logger writes plain text to stderr, so Cloud Run stores each line in
# textPayload; ":" is a substring match. TestMonitoringLogMarkersMatchGo
# (terraform/modules/monitoring/log_marker_contract_test.go) keeps the tokens
# in step with the Go code.

locals {
  failure_markers = {
    purchase_failed = {
      token        = "purchase_execution_failed"
      display_name = "${var.service_name} purchase execution failed"
    }
    scheduled_task_failed = {
      token        = "scheduled_task_failed"
      display_name = "${var.service_name} scheduled task failed"
    }
  }

  alert_notification_channels = concat(
    google_monitoring_notification_channel.email[*].id,
    var.slack_webhook_url != "" ? [google_monitoring_notification_channel.slack[0].id] : []
  )
}

resource "google_logging_metric" "failure_marker" {
  for_each = local.failure_markers

  name   = "${var.service_name}_${each.key}"
  filter = <<-EOT
    resource.type="cloud_run_revision"
    resource.labels.service_name="${var.service_name}"
    textPayload:"${each.value.token}"
  EOT

  metric_descriptor {
    metric_kind = "DELTA"
    value_type  = "INT64"
    unit        = "1"
  }
}

resource "google_monitoring_alert_policy" "failure_marker" {
  for_each = local.failure_markers

  display_name = each.value.display_name
  combiner     = "OR"

  conditions {
    display_name = "${each.value.token} logged"

    condition_threshold {
      filter          = "resource.type=\"cloud_run_revision\" AND metric.type=\"logging.googleapis.com/user/${google_logging_metric.failure_marker[each.key].name}\""
      duration        = "0s"
      comparison      = "COMPARISON_GT"
      threshold_value = 0

      aggregations {
        alignment_period   = "300s"
        per_series_aligner = "ALIGN_SUM"
      }
    }
  }

  documentation {
    content   = "Search Cloud Run logs for `${each.value.token}`: purchase failures carry the execution_id, task failures the task name and error."
    mime_type = "text/markdown"
  }

  notification_channels = local.alert_notification_channels

  alert_strategy {
    auto_close = "1800s"
  }

  user_labels = var.labels
}
