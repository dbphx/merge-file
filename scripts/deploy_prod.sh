#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ENV_FILE:-$ROOT_DIR/.env}"
COMPOSE_FILE="$ROOT_DIR/docker-compose.prod.yml"
COMPOSE_CMD=(docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE")

usage() {
  cat <<'EOF'
Usage:
  ./scripts/deploy_prod.sh up
  ./scripts/deploy_prod.sh down
  ./scripts/deploy_prod.sh restart
  ./scripts/deploy_prod.sh logs [service]
  ./scripts/deploy_prod.sh ps

Options:
  Set ENV_FILE=/path/to/.env to use a custom environment file.
EOF
}

require_env_file() {
  if [[ ! -f "$ENV_FILE" ]]; then
    echo "Missing env file: $ENV_FILE" >&2
    exit 1
  fi
}

load_env() {
  require_env_file
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
}

require_vars() {
  local missing=()
  local required=(
    APP_URL
    CLOUDFLARED_DIR
    JWT_SECRET
    POSTGRES_PASSWORD
    MINIO_ROOT_PASSWORD
  )

  for name in "${required[@]}"; do
    if [[ -z "${!name:-}" ]]; then
      missing+=("$name")
    fi
  done

  if (( ${#missing[@]} > 0 )); then
    echo "Missing required env vars in $ENV_FILE: ${missing[*]}" >&2
    exit 1
  fi
}

require_cloudflared_config() {
  local config_dir config_file

  config_dir="${CLOUDFLARED_DIR}"
  config_file="${config_dir%/}/config.yml"

  if [[ ! -d "$config_dir" ]]; then
    echo "Missing Cloudflare directory: $config_dir" >&2
    exit 1
  fi

  if [[ ! -f "$config_file" ]]; then
    echo "Missing Cloudflare config file: $config_file" >&2
    echo "See cloudflared/config.example.yml for the expected structure." >&2
    exit 1
  fi
}

wait_for_postgres() {
  local container_id
  container_id="$("${COMPOSE_CMD[@]}" ps -q postgres)"
  if [[ -z "$container_id" ]]; then
    echo "Postgres container is not running" >&2
    exit 1
  fi

  echo "Waiting for postgres healthcheck..."
  for _ in $(seq 1 60); do
    local status
    status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container_id")"
    if [[ "$status" == "healthy" ]]; then
      return 0
    fi
    sleep 2
  done

  echo "Postgres did not become healthy in time" >&2
  exit 1
}

run_migrations() {
  local postgres_user postgres_db migration

  postgres_user="${POSTGRES_USER:-mergepdf}"
  postgres_db="${POSTGRES_DB:-mergepdf}"

  echo "Applying schema migrations..."
  for migration in \
    "$ROOT_DIR/migrations/002_add_job_progress.sql" \
    "$ROOT_DIR/migrations/003_add_job_runtime_state.sql" \
    "$ROOT_DIR/migrations/004_add_job_file_transfer_state.sql" \
    "$ROOT_DIR/migrations/005_add_job_file_source_object_key.sql" \
    "$ROOT_DIR/migrations/006_add_catalogs.sql"; do
    echo "  -> $(basename "$migration")"
    "${COMPOSE_CMD[@]}" exec -T postgres \
      psql -U "$postgres_user" -d "$postgres_db" -v ON_ERROR_STOP=1 < "$migration"
  done
}

cmd_up() {
  load_env
  require_vars
  require_cloudflared_config
  "${COMPOSE_CMD[@]}" up -d --build
  wait_for_postgres
  run_migrations
  "${COMPOSE_CMD[@]}" ps
}

cmd_down() {
  require_env_file
  "${COMPOSE_CMD[@]}" down
}

cmd_restart() {
  cmd_down
  cmd_up
}

cmd_logs() {
  require_env_file
  if [[ $# -gt 0 ]]; then
    "${COMPOSE_CMD[@]}" logs -f "$1"
    return
  fi
  "${COMPOSE_CMD[@]}" logs -f
}

cmd_ps() {
  require_env_file
  "${COMPOSE_CMD[@]}" ps
}

main() {
  local command="${1:-up}"
  shift || true

  case "$command" in
    up)
      cmd_up
      ;;
    down)
      cmd_down
      ;;
    restart)
      cmd_restart
      ;;
    logs)
      cmd_logs "$@"
      ;;
    ps)
      cmd_ps
      ;;
    -h|--help|help)
      usage
      ;;
    *)
      usage
      exit 1
      ;;
  esac
}

main "$@"
