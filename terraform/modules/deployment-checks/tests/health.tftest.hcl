# The post-deploy health gate must fail on a degraded deployment (#649).
# /health answers HTTP 200 even when every dependency is down (liveness
# contract, #488); the verdict is the body's top-level "status" field.
#
# Every check block reads a mocked http data source. Only data.http.health is
# overridden per run; the others share the mock default (HTTP 200, "{}"), so
# the checks that expect a non-200 status always fail here and are listed in
# expect_failures. They are not what this file tests.
mock_provider "http" {
  mock_data "http" {
    defaults = {
      status_code      = 200
      response_body    = "{}"
      response_headers = {}
    }
  }
}

variables {
  api_base_url  = "https://app.example.com"
  provider_name = "gcp"
}

run "healthy_body_passes" {
  command = plan

  override_data {
    target = data.http.health
    values = {
      status_code   = 200
      response_body = "{\"timestamp\":\"2026-01-01T00:00:00Z\",\"checks\":{\"config_store\":{\"status\":\"healthy\"}},\"status\":\"healthy\",\"version\":\"test\"}"
    }
  }

  expect_failures = [
    check.security_headers,
    check.auth_enforcement,
    check.error_empty_body,
    check.error_invalid_json,
    check.method_post_summary,
    check.content_type_xml,
    check.content_type_missing,
    check.request_size_limit,
    check.frontend_serving,
  ]
}

# The case the issue describes: the database is unreachable, /health still
# answers 200, and the nested checks still contain the string "healthy".
run "degraded_body_with_200_fails" {
  command = plan

  override_data {
    target = data.http.health
    values = {
      status_code   = 200
      response_body = "{\"timestamp\":\"2026-01-01T00:00:00Z\",\"checks\":{\"auth_store\":{\"status\":\"healthy\"},\"config_store\":{\"status\":\"unhealthy\",\"message\":\"config store unavailable\"}},\"status\":\"degraded\",\"version\":\"test\"}"
    }
  }

  expect_failures = [
    check.health,
    check.security_headers,
    check.auth_enforcement,
    check.error_empty_body,
    check.error_invalid_json,
    check.method_post_summary,
    check.content_type_xml,
    check.content_type_missing,
    check.request_size_limit,
    check.frontend_serving,
  ]
}

# A 200 whose body is not the health JSON (e.g. an SPA index served by a
# catch-all route) must not pass either.
run "non_json_body_fails" {
  command = plan

  override_data {
    target = data.http.health
    values = {
      status_code   = 200
      response_body = "<!doctype html><title>healthy</title>"
    }
  }

  expect_failures = [
    check.health,
    check.security_headers,
    check.auth_enforcement,
    check.error_empty_body,
    check.error_invalid_json,
    check.method_post_summary,
    check.content_type_xml,
    check.content_type_missing,
    check.request_size_limit,
    check.frontend_serving,
  ]
}

run "non_200_fails" {
  command = plan

  override_data {
    target = data.http.health
    values = {
      status_code   = 503
      response_body = "{\"status\":\"healthy\"}"
    }
  }

  expect_failures = [
    check.health,
    check.security_headers,
    check.auth_enforcement,
    check.error_empty_body,
    check.error_invalid_json,
    check.method_post_summary,
    check.content_type_xml,
    check.content_type_missing,
    check.request_size_limit,
    check.frontend_serving,
  ]
}
