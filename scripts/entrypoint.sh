#!/bin/sh
set -eu

# ==============================================
# Unified Entrypoint for Multi-Cloud Deployment
# Supports: AWS Lambda, AWS Fargate, GCP Cloud Run, Azure Container Apps
# ==============================================

echo "🚀 CUDly starting..."
echo "   Environment: ${ENVIRONMENT:-production}"
echo "   Runtime Mode: ${RUNTIME_MODE:-auto}"

# Auto-detect runtime environment if set to 'auto'
if [ "$RUNTIME_MODE" = "auto" ]; then
  if [ -n "${AWS_LAMBDA_RUNTIME_API:-}" ]; then
    echo "   Detected: AWS Lambda"
    RUNTIME_MODE=lambda
  elif [ -n "${K_SERVICE:-}" ]; then
    echo "   Detected: GCP Cloud Run"
    RUNTIME_MODE=http
  elif [ -n "${CONTAINER_APP_NAME:-}" ]; then
    echo "   Detected: Azure Container Apps"
    RUNTIME_MODE=http
  elif [ -n "${ECS_CONTAINER_METADATA_URI:-}" ]; then
    echo "   Detected: AWS ECS Fargate"
    RUNTIME_MODE=http
  else
    echo "   Detected: Standard container (defaulting to HTTP mode)"
    RUNTIME_MODE=http
  fi
fi

echo "   Starting in: $RUNTIME_MODE mode"

# ==============================================
# Database Migrations
# ==============================================

if [ "$DB_AUTO_MIGRATE" = "true" ]; then
  echo "📦 Running database migrations..."

  # Build database connection string
  DB_HOST=${DB_HOST:-localhost}
  DB_PORT=${DB_PORT:-5432}
  DB_NAME=${DB_NAME:-cudly}
  DB_USER=${DB_USER:-cudly}
  DB_SSL_MODE=${DB_SSL_MODE:-require}

  # Get database password from secret or environment. Both variables default
  # to empty: under `set -u` a bare expansion aborts the container when they
  # are unset (e.g. the docker-compose test environment, which passes
  # DB_PASSWORD directly).
  if [ -n "${DB_PASSWORD_SECRET:-}" ] && [ "${SECRET_PROVIDER:-}" != "env" ]; then
    echo "   Resolving DB password from secret manager..."
    # Password will be resolved by the application
    # Migration will use the environment variable if available
    if [ -z "${DB_PASSWORD:-}" ]; then
      echo "   ⚠️  DB_PASSWORD not set, migrations may fail"
      echo "   Application will attempt to resolve from secret manager on startup"
    fi
  fi

  if [ -n "${DB_PASSWORD:-}" ]; then
    # Run migrations if password is available.
    # The password goes to migrate through PGPASSWORD, not the URL: pgx reads
    # it when the URL has no password, so no percent-encoding is needed for
    # passwords with @ : / ? # % or spaces (generated ones contain =, [, $,
    # etc.), and the secret stays out of migrate's command line. It is set
    # for the migrate command only, not exported to the application.
    # pgx5://, not postgresql://: the migrate binary in this image is built
    # with -tags=pgx5 (see Dockerfile), and golang-migrate dispatches on the
    # URL scheme. The pgx/v5 driver registers "pgx5" only. See issue #1849.
    DB_URL="pgx5://${DB_USER}@${DB_HOST}:${DB_PORT}/${DB_NAME}?sslmode=${DB_SSL_MODE}"

    # `|| MIGRATE_EXIT_CODE=$?` keeps the failure out of errexit so the
    # diagnostic below runs. golang-migrate exits 0 on "no change", so any
    # non-zero status is a real failure.
    MIGRATE_EXIT_CODE=0
    PGPASSWORD="$DB_PASSWORD" migrate -path "$DB_MIGRATIONS_PATH" -database "$DB_URL" up 2>&1 || MIGRATE_EXIT_CODE=$?
    if [ "$MIGRATE_EXIT_CODE" -ne 0 ]; then
      echo "   ❌ Migration failed with exit code $MIGRATE_EXIT_CODE"
      exit "$MIGRATE_EXIT_CODE"
    fi
    echo "   ✅ Migrations completed successfully"
  else
    echo "   ⚠️  Skipping migrations (DB_PASSWORD not available)"
    echo "   Application will handle migrations on first connection"
  fi
else
  echo "📦 Skipping database migrations (DB_AUTO_MIGRATE=false)"
fi

# ==============================================
# Start Application
# ==============================================

case $RUNTIME_MODE in
  lambda)
    echo "🔷 Starting AWS Lambda handler..."
    echo "   Lambda Runtime API: $AWS_LAMBDA_RUNTIME_API"
    echo "   Handler: /app/cudly --mode=lambda"

    # Lambda mode: Use AWS Lambda Runtime Interface
    exec "$@" --mode=lambda
    ;;

  http)
    echo "🌐 Starting HTTP server..."
    echo "   Port: ${PORT:-8080}"
    echo "   Handler: /app/cudly --mode=http"

    # HTTP mode: Standard HTTP server for Fargate, Cloud Run, Container Apps
    exec "$@" --mode=http --port="${PORT:-8080}"
    ;;

  *)
    echo "❌ Unknown RUNTIME_MODE: $RUNTIME_MODE"
    echo "   Valid modes: lambda, http, auto"
    exit 1
    ;;
esac
