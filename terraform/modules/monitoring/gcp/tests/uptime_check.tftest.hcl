# The uptime check must tell "process alive" from "serving traffic" (#649).
# /health answers HTTP 200 even when every dependency is down (liveness
# contract, #488), so the check has to match the body's top-level "status"
# exactly. A CONTAINS_STRING "healthy" matcher also matches "unhealthy" and
# every nested check's {"status":"healthy"}, so it passed on a degraded replica.
mock_provider "google" {}

variables {
  project_id     = "example-project"
  service_name   = "cudly"
  environment    = "dev"
  db_instance_id = "cudly-db"
  service_url    = "cudly.example.com"
}

run "uptime_check_matches_top_level_status" {
  command = plan

  assert {
    condition     = google_monitoring_uptime_check_config.service_health.http_check[0].path == "/health"
    error_message = "The uptime check must stay on /health (liveness); /ready answers 503 during every rollout and would flap the alert."
  }

  assert {
    condition     = length(google_monitoring_uptime_check_config.service_health.content_matchers) == 1
    error_message = "The uptime check needs exactly one content matcher on the /health body."
  }

  assert {
    condition = alltrue([
      for m in google_monitoring_uptime_check_config.service_health.content_matchers :
      m.matcher == "MATCHES_JSON_PATH" && m.content == "healthy" &&
      length(m.json_path_matcher) == 1 &&
      m.json_path_matcher[0].json_path == "$.status" &&
      m.json_path_matcher[0].json_matcher == "EXACT_MATCH"
    ])
    error_message = "The uptime check must exact-match \"healthy\" at JSON path $.status; a substring match on \"healthy\" also passes a degraded body."
  }
}
