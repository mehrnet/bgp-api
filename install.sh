#!/usr/bin/env bash
# Install, update, or remove the digest-pinned MehrNet BGP API Docker deployment.
set -Eeuo pipefail

readonly REPOSITORY="${BGP_API_GITHUB_REPOSITORY:-mehrnet/bgp-api}"
readonly INSTALL_DIR="${BGP_API_INSTALL_DIR:-/srv/bgp-api}"
readonly ENV_FILE="$INSTALL_DIR/.env"
readonly COMPOSE_FILE="$INSTALL_DIR/docker-compose.yml"
readonly SYNC_SCRIPT="$INSTALL_DIR/scripts/sync-docker.sh"
readonly BARE_SYNC_SCRIPT="$INSTALL_DIR/scripts/sync-postgres.sh"
readonly INSTALLED_SCRIPT="$INSTALL_DIR/install.sh"
readonly CONFIG_FILE="$INSTALL_DIR/install.conf"
readonly BARE_ENV_FILE="/etc/bgp-api/postgres.env"
readonly BARE_SERVICE_FILE="/etc/systemd/system/bgp-api.service"
readonly CRON_FILE="/etc/cron.d/mehrnet-bgp-api-update"
readonly LOCK_FILE="/run/lock/mehrnet-bgp-api-install.lock"

ACTION=install
DEPLOY_MODE=docker
DEPLOY_MODE_SET=false
DOMAIN=""
DOMAIN_SET=false
AUTO_UPDATE=false

if [ -t 1 ]; then
  readonly BOLD=$'\033[1m'
  readonly CYAN=$'\033[36m'
  readonly GREEN=$'\033[32m'
  readonly RESET=$'\033[0m'
else
  readonly BOLD="" CYAN="" GREEN="" RESET=""
fi

say() { printf '%s\n' "$*"; }
step() { printf '\n%s%s==>%s %s\n' "$BOLD" "$CYAN" "$RESET" "$*"; }
success() { printf '%s%s%s %s\n' "$GREEN" "$BOLD" "$*" "$RESET"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Install, update, or uninstall MehrNet BGP API.

Usage:
  install.sh --mode docker|bare [--domain DOMAIN] [--auto-update]
  install.sh [--mode docker|bare] --update
  install.sh [--mode docker|bare] --uninstall

Options:
  --mode MODE      Deployment mode: docker (default) or bare.
  --domain DOMAIN  Configure Caddy and HTTPS for the given API hostname.
  --auto-update    Install a daily update job for exactly 06:00 UTC.
  --update         Apply verified release patches and update the API image.
  --uninstall      Remove the deployment, database, images, Caddy config, and cron job.
  -h, --help       Show this help.

Environment:
  BGP_API_INSTALL_DIR        Installation directory (default: /srv/bgp-api)
  BGP_API_GITHUB_TOKEN       Optional GitHub token for a higher API rate limit
  BGP_API_GITHUB_REPOSITORY  Release repository (default: mehrnet/bgp-api)
  BGP_API_SOURCE_REF         Repository ref for deployment files (default: main)
  BGP_API_SKIP_DISK_CHECK=1  Skip the initial free-space check
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --mode)
      [ "$#" -ge 2 ] || die "--mode requires docker or bare"
      DEPLOY_MODE="${2,,}"
      DEPLOY_MODE_SET=true
      shift 2
      ;;
    --domain)
      [ "$#" -ge 2 ] || die "--domain requires a hostname"
      DOMAIN="$2"
      DOMAIN_SET=true
      shift 2
      ;;
    --auto-update|--audo-update)
      AUTO_UPDATE=true
      shift
      ;;
    --update)
      [ "$ACTION" = install ] || die "only one operation can be selected"
      ACTION=update
      shift
      ;;
    --uninstall)
      [ "$ACTION" = install ] || die "only one operation can be selected"
      ACTION=uninstall
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *) die "unknown option: $1 (use --help)" ;;
  esac
done

[ "$EUID" -eq 0 ] || die "run this installer as root (for example, pipe it to sudo bash)"
[ "$(uname -s)" = Linux ] || die "this installer requires Linux"
[[ "$INSTALL_DIR" != *[[:space:]]* ]] || die "BGP_API_INSTALL_DIR cannot contain whitespace"
case "$DEPLOY_MODE" in
  docker|bare) ;;
  *) die "--mode must be docker or bare" ;;
esac
if [ "$ACTION" != install ] && { [ "$DOMAIN_SET" = true ] || [ "$AUTO_UPDATE" = true ]; }; then
  die "--$ACTION cannot be combined with installation options"
fi

valid_domain() {
  [[ "$1" =~ ^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z]{2,63}$ ]]
}

if [ "$DOMAIN_SET" = true ]; then
  DOMAIN="${DOMAIN,,}"
  valid_domain "$DOMAIN" || die "invalid domain: $DOMAIN"
fi

package_manager() {
  for manager in apt-get dnf yum pacman zypper apk; do
    if command -v "$manager" >/dev/null 2>&1; then
      printf '%s\n' "$manager"
      return
    fi
  done
  return 1
}

install_base_tools() {
  local missing=false command manager
  for command in curl gzip jq openssl sha256sum tar flock awk sed grep install; do
    command -v "$command" >/dev/null 2>&1 || missing=true
  done
  [ "$missing" = false ] && return
  manager="$(package_manager)" || die "install curl, gzip, jq, openssl, coreutils, tar, and util-linux, then retry"
  step "Installing required command-line tools"
  case "$manager" in
    apt-get)
      apt-get update
      DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl gzip jq openssl coreutils tar util-linux
      ;;
    dnf) dnf install -y ca-certificates curl gzip jq openssl coreutils tar util-linux ;;
    yum) yum install -y ca-certificates curl gzip jq openssl coreutils tar util-linux ;;
    pacman) pacman -Sy --noconfirm ca-certificates curl gzip jq openssl coreutils tar util-linux ;;
    zypper) zypper --non-interactive install ca-certificates curl gzip jq openssl coreutils tar util-linux ;;
    apk) apk add --no-cache ca-certificates curl gzip jq openssl coreutils tar util-linux ;;
  esac
}

install_docker() {
  local manager installer
  if ! command -v docker >/dev/null 2>&1; then
    step "Installing Docker Engine and Compose"
    manager="$(package_manager)" || die "Docker is missing and no supported package manager was found"
    case "$manager" in
      pacman)
        pacman -Sy --noconfirm docker docker-compose
        ;;
      apk)
        apk add --no-cache docker docker-cli-compose
        ;;
      *)
        installer="$(mktemp)"
        curl --fail --location --silent --show-error --retry 3 \
          https://get.docker.com --output "$installer"
        sh "$installer"
        rm -f "$installer"
        ;;
    esac
  fi
  if command -v systemctl >/dev/null 2>&1; then
    systemctl enable --now docker >/dev/null
  elif command -v rc-service >/dev/null 2>&1; then
    rc-update add docker default >/dev/null 2>&1 || true
    rc-service docker start >/dev/null
  elif ! docker info >/dev/null 2>&1; then
    die "Docker is installed but its daemon is not running"
  fi
  docker compose version >/dev/null 2>&1 || die "the Docker Compose plugin is required"
  docker info >/dev/null 2>&1 || die "cannot connect to the Docker daemon"
}

install_postgresql() {
  local manager data_dir postgres_unit
  command -v systemctl >/dev/null 2>&1 || die "bare mode requires a systemd-based Linux host"
  if ! command -v psql >/dev/null 2>&1 || ! command -v pg_isready >/dev/null 2>&1; then
    manager="$(package_manager)" || die "PostgreSQL is missing and no supported package manager was found"
    step "Installing PostgreSQL"
    case "$manager" in
      apt-get)
        apt-get update
        DEBIAN_FRONTEND=noninteractive apt-get install -y postgresql postgresql-client
        ;;
      dnf|yum)
        "$manager" install -y postgresql postgresql-server
        if [ ! -s /var/lib/pgsql/data/PG_VERSION ]; then
          postgresql-setup --initdb
        fi
        ;;
      pacman)
        pacman -Sy --noconfirm postgresql
        data_dir=/var/lib/postgres/data
        if [ ! -s "$data_dir/PG_VERSION" ]; then
          install -d -m 0700 -o postgres -g postgres "$data_dir"
          runuser -u postgres -- initdb --locale=C.UTF-8 --encoding=UTF8 -D "$data_dir"
        fi
        ;;
      zypper) zypper --non-interactive install postgresql postgresql-server ;;
      *) die "bare mode does not support PostgreSQL installation with $manager" ;;
    esac
  fi
  if systemctl list-unit-files postgresql.service --no-legend 2>/dev/null | grep -q '^postgresql\.service'; then
    systemctl enable --now postgresql >/dev/null
  else
    postgres_unit="$(systemctl list-unit-files --type=service --no-legend 'postgresql*.service' | awk 'NR == 1 { print $1 }')"
    [ -n "$postgres_unit" ] || die "could not locate the PostgreSQL systemd service"
    systemctl enable --now "$postgres_unit" >/dev/null
  fi
  for _ in {1..30}; do
    pg_isready -q && return
    sleep 1
  done
  die "PostgreSQL did not become ready within 30 seconds"
}

postgres_admin() {
  runuser -u postgres -- psql --set ON_ERROR_STOP=1 "$@"
}

github_api() {
  local -a headers=(-H 'Accept: application/vnd.github+json')
  if [ -n "${BGP_API_GITHUB_TOKEN:-}" ]; then
    headers+=(-H "Authorization: Bearer $BGP_API_GITHUB_TOKEN")
  fi
  curl --fail --location --silent --show-error --retry 3 \
    "${headers[@]}" "https://api.github.com$1"
}

download() {
  curl --fail --location --silent --show-error --retry 3 --output "$2" "$1"
}

env_value() {
  local key="$1" file="$2"
  [ -f "$file" ] || return 0
  awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; exit }' "$file"
}

installed_mode() {
  local mode
  mode="$(env_value MODE "$CONFIG_FILE")"
  if [ -z "$mode" ]; then
    if [ -f "$ENV_FILE" ] && [ -f "$COMPOSE_FILE" ]; then
      mode=docker
    elif [ -f "$BARE_ENV_FILE" ] && [ -f "$BARE_SERVICE_FILE" ]; then
      mode=bare
    fi
  fi
  printf '%s\n' "$mode"
}

resolve_installed_mode() {
  local detected
  detected="$(installed_mode)"
  case "$detected" in
    docker|bare) ;;
    *) die "no bgp-api installation found in $INSTALL_DIR" ;;
  esac
  if [ "$DEPLOY_MODE_SET" = true ] && [ "$DEPLOY_MODE" != "$detected" ]; then
    die "the installed mode is $detected, not $DEPLOY_MODE"
  fi
  DEPLOY_MODE="$detected"
}

compose() {
  docker compose --env-file "$ENV_FILE" --file "$COMPOSE_FILE" "$@"
}

install_cron() {
  local manager service=""
  if ! command -v cron >/dev/null 2>&1 && ! command -v crond >/dev/null 2>&1; then
    manager="$(package_manager)" || die "a cron daemon is required for --auto-update"
    step "Installing the automatic-update scheduler"
    case "$manager" in
      apt-get)
        apt-get update
        DEBIAN_FRONTEND=noninteractive apt-get install -y cron
        ;;
      dnf|yum) "$manager" install -y cronie ;;
      pacman) pacman -Sy --noconfirm cronie ;;
      zypper) zypper --non-interactive install cron ;;
      apk) apk add --no-cache dcron ;;
    esac
  fi
  cat > "$CRON_FILE" <<EOF
SHELL=/bin/bash
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
CRON_TZ=UTC

0 6 * * * root $INSTALLED_SCRIPT --mode $DEPLOY_MODE --update >> /var/log/mehrnet-bgp-api-update.log 2>&1
EOF
  chmod 0644 "$CRON_FILE"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl list-unit-files cron.service >/dev/null 2>&1 && service=cron
    systemctl list-unit-files crond.service >/dev/null 2>&1 && service=crond
    [ -n "$service" ] && systemctl enable --now "$service" >/dev/null
  elif command -v rc-service >/dev/null 2>&1; then
    rc-update add crond default >/dev/null 2>&1 || true
    rc-service crond start >/dev/null
  else
    die "cron was configured but its service manager could not be detected"
  fi
}

install_caddy_package() {
  local manager
  command -v caddy >/dev/null 2>&1 && return
  manager="$(package_manager)" || die "Caddy is missing and no supported package manager was found"
  step "Installing Caddy"
  case "$manager" in
    apt-get)
      apt-get update
      DEBIAN_FRONTEND=noninteractive apt-get install -y caddy
      ;;
    dnf|yum) "$manager" install -y caddy ;;
    pacman) pacman -Sy --noconfirm caddy ;;
    zypper) zypper --non-interactive install caddy ;;
    apk) apk add --no-cache caddy ;;
  esac
  command -v caddy >/dev/null 2>&1 || die "Caddy could not be installed automatically"
}

configure_caddy() {
  local token="$1" main_config=/etc/caddy/Caddyfile managed_config=/etc/caddy/conf.d/mehrnet-bgp-api.caddy
  install_caddy_package
  command -v systemctl >/dev/null 2>&1 || die "Caddy domain setup requires systemd"
  mkdir -p /etc/caddy/conf.d /etc/bgp-api /etc/systemd/system/caddy.service.d
  cat > "$managed_config" <<EOF
$DOMAIN {
	encode zstd gzip

	@cloudflare remote_ip 173.245.48.0/20 103.21.244.0/22 103.22.200.0/22 103.31.4.0/22 141.101.64.0/18 108.162.192.0/18 190.93.240.0/20 188.114.96.0/20 197.234.240.0/22 198.41.128.0/17 162.158.0.0/15 104.16.0.0/13 104.24.0.0/14 172.64.0.0/13 131.0.72.0/22 2400:cb00::/32 2606:4700::/32 2803:f800::/32 2405:b500::/32 2405:8100::/32 2a06:98c0::/29 2c0f:f248::/32
	handle @cloudflare {
		reverse_proxy 127.0.0.1:3102 {
			header_up X-BGP-API-Origin-Token {env.ORIGIN_AUTH_TOKEN}
			header_up X-BGP-API-Cloudflare-IPv6 {http.request.header.CF-Connecting-IPv6}
			header_up X-BGP-API-Cloudflare-IP {http.request.header.CF-Connecting-IP}
			header_up X-BGP-API-Forwarded-IP {http.request.remote.host}
		}
	}

	handle {
		reverse_proxy 127.0.0.1:3102 {
			header_up X-BGP-API-Origin-Token {env.ORIGIN_AUTH_TOKEN}
			header_up X-BGP-API-Cloudflare-IPv6 {http.request.remote.host}
			header_up X-BGP-API-Cloudflare-IP {http.request.remote.host}
			header_up X-BGP-API-Forwarded-IP {http.request.remote.host}
		}
	}
}
EOF
  touch "$main_config"
  grep -Fqx 'import /etc/caddy/conf.d/*.caddy' "$main_config" || \
    printf '\nimport /etc/caddy/conf.d/*.caddy\n' >> "$main_config"
  printf 'ORIGIN_AUTH_TOKEN=%s\n' "$token" > /etc/bgp-api/caddy.env
  chmod 0640 /etc/bgp-api/caddy.env
  if command -v getent >/dev/null 2>&1 && getent group caddy >/dev/null 2>&1; then
    chown root:caddy /etc/bgp-api/caddy.env
  fi
  cat > /etc/systemd/system/caddy.service.d/mehrnet-bgp-api.conf <<'EOF'
[Service]
EnvironmentFile=/etc/bgp-api/caddy.env
EOF
  caddy fmt --overwrite "$managed_config"
  ORIGIN_AUTH_TOKEN="$token" caddy validate --config "$main_config" --adapter caddyfile
  systemctl daemon-reload
  systemctl enable caddy >/dev/null
  systemctl restart caddy
}

wait_for_api() {
  local token="$1"
  step "Waiting for PostgreSQL and the API"
  for _ in {1..120}; do
    if [ -n "$token" ]; then
      curl --fail --silent --show-error \
        -H "X-BGP-API-Origin-Token: $token" \
        http://127.0.0.1:3102/v1/health >/dev/null 2>&1 && return
    else
      curl --fail --silent --show-error \
        http://127.0.0.1:3102/v1/health >/dev/null 2>&1 && return
    fi
    sleep 2
  done
  if [ "$DEPLOY_MODE" = docker ]; then
    compose ps >&2 || true
    compose logs --tail 50 api postgres >&2 || true
  else
    systemctl status bgp-api --no-pager >&2 || true
    journalctl -u bgp-api -n 50 --no-pager >&2 || true
  fi
  die "the API did not become healthy within four minutes"
}

install_bare_deployment() {
  local arch database_exists role_exists db_password origin_token source_ref source_sha release_tag
  local bare_work_dir available_kib required_kib

  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "bare mode supports Linux amd64 and arm64 hosts" ;;
  esac
  command -v useradd >/dev/null 2>&1 || die "bare mode requires useradd"
  install_postgresql
  if [ "${BGP_API_SKIP_DISK_CHECK:-0}" != 1 ]; then
    available_kib="$(df -Pk /var/lib | awk 'NR == 2 { print $4 }')"
    required_kib=$((18 * 1024 * 1024))
    [ "$available_kib" -ge "$required_kib" ] || \
      die "bare mode needs at least 18 GiB free to import and index the database"
  fi

  database_exists="$(postgres_admin --dbname postgres --tuples-only --no-align \
    --command "SELECT 1 FROM pg_database WHERE datname = 'bgp_api';")"
  role_exists="$(postgres_admin --dbname postgres --tuples-only --no-align \
    --command "SELECT 1 FROM pg_roles WHERE rolname = 'bgp_api';")"
  [ -z "$database_exists" ] || die "PostgreSQL database bgp_api already exists"
  [ -z "$role_exists" ] || die "PostgreSQL role bgp_api already exists"

  step "Creating the bare-metal PostgreSQL database"
  db_password="$(openssl rand -hex 32)"
  postgres_admin --dbname postgres --set "db_password=$db_password" <<'SQL'
CREATE ROLE bgp_api LOGIN PASSWORD :'db_password';
CREATE DATABASE bgp_api OWNER bgp_api;
SQL
  if ! id -u bgpapi >/dev/null 2>&1; then
    useradd --system --home-dir /var/lib/bgp-api --create-home --shell /usr/sbin/nologin bgpapi
  fi
  install -d -m 0750 -o bgpapi -g bgpapi /var/lib/bgp-api
  install -d -m 0755 -o root -g root /etc/bgp-api "$INSTALL_DIR/scripts"

  step "Installing the bare-metal deployment files"
  bare_work_dir="$(mktemp -d)"
  cleanup_bare() {
    [ -z "${bare_work_dir:-}" ] || rm -rf -- "$bare_work_dir"
  }
  trap cleanup_bare EXIT
  source_ref="${BGP_API_SOURCE_REF:-main}"
  source_sha="$(github_api "/repos/$REPOSITORY/commits/$source_ref" | jq -r '.sha // empty')"
  [[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || die "could not resolve repository source ref $source_ref"
  download "https://raw.githubusercontent.com/$REPOSITORY/$source_sha/scripts/sync-postgres.sh" "$bare_work_dir/sync-postgres.sh"
  download "https://raw.githubusercontent.com/$REPOSITORY/$source_sha/deploy/bgp-api.service" "$bare_work_dir/bgp-api.service"
  download "https://raw.githubusercontent.com/$REPOSITORY/$source_sha/install.sh" "$bare_work_dir/install.sh"
  install -m 0755 "$bare_work_dir/sync-postgres.sh" "$BARE_SYNC_SCRIPT"
  install -m 0755 "$bare_work_dir/install.sh" "$INSTALLED_SCRIPT"
  install -m 0644 "$bare_work_dir/bgp-api.service" "$BARE_SERVICE_FILE"

  origin_token=""
  if [ "$DOMAIN_SET" = true ]; then
    origin_token="$(openssl rand -hex 32)"
  fi
  cat > "$BARE_ENV_FILE" <<EOF
DATABASE_URL=postgresql://bgp_api:$db_password@127.0.0.1:5432/bgp_api?sslmode=disable
LISTEN_ADDR=127.0.0.1:3102
POSTGRES_MAX_CONNECTIONS=8
ORIGIN_AUTH_TOKEN=$origin_token
TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128
CORS_ALLOWED_ORIGINS_JSON='["https://bgp.mehrnet.com"]'
BGP_API_GITHUB_REPOSITORY=$REPOSITORY
BGP_API_GITHUB_TOKEN=${BGP_API_GITHUB_TOKEN:-}
EOF
  chown root:bgpapi "$BARE_ENV_FILE"
  chmod 0640 "$BARE_ENV_FILE"
  {
    printf 'MODE=bare\n'
    printf 'DOMAIN=%s\n' "$DOMAIN"
  } > "$CONFIG_FILE"
  chmod 0600 "$CONFIG_FILE"

  systemctl daemon-reload
  step "Importing and indexing the latest PostgreSQL snapshot"
  DATABASE_URL="postgresql://bgp_api:$db_password@127.0.0.1:5432/bgp_api?sslmode=disable" \
    BGP_API_DATABASE_ROLE=bgp_api \
    BGP_API_GITHUB_REPOSITORY="$REPOSITORY" \
    BGP_API_GITHUB_TOKEN="${BGP_API_GITHUB_TOKEN:-}" \
    BGP_API_SYNC_MODE=snapshot \
    BGP_API_BINARY_PATH=/usr/local/bin/bgp-api \
    BGP_API_SERVICE_NAME=bgp-api \
    "$BARE_SYNC_SCRIPT"
  systemctl enable --now bgp-api >/dev/null
  wait_for_api "$origin_token"

  if [ "$DOMAIN_SET" = true ]; then
    configure_caddy "$origin_token"
  fi
  if [ "$AUTO_UPDATE" = true ]; then
    install_cron
  fi
  release_tag="$(postgres_admin --dbname bgp_api --tuples-only --no-align \
    --command 'SELECT release_tag FROM public.bgp_api_dataset WHERE singleton;')"

  say
  success "MehrNet BGP API bare-metal installation completed."
  say "Release: $release_tag"
  say "Architecture: linux/$arch"
  say "Local health: http://127.0.0.1:3102/v1/health"
  [ "$DOMAIN_SET" = false ] || say "Public health: https://$DOMAIN/v1/health"
  say "Update: $INSTALLED_SCRIPT --mode bare --update"
  say "Logs: journalctl -u bgp-api -f"
  [ "$AUTO_UPDATE" = false ] || say "Automatic updates: daily at 06:00 UTC via $CRON_FILE"
  cleanup_bare
  trap - EXIT
}

uninstall_deployment() {
  local managed_caddy=/etc/caddy/conf.d/mehrnet-bgp-api.caddy
  local caddy_main=/etc/caddy/Caddyfile
  local image
  local -a deployment_images=()

  resolve_installed_mode
  [[ "$INSTALL_DIR" == /*/* ]] || die "refusing unsafe installation path: $INSTALL_DIR"

  step "Stopping and removing the $DEPLOY_MODE deployment"
  if [ "$DEPLOY_MODE" = docker ]; then
    if command -v docker >/dev/null 2>&1; then
      if ! docker info >/dev/null 2>&1 && command -v systemctl >/dev/null 2>&1; then
        systemctl start docker >/dev/null 2>&1 || true
      fi
      docker info >/dev/null 2>&1 || die "start Docker before uninstalling bgp-api"
      docker compose version >/dev/null 2>&1 || die "the Docker Compose plugin is required to uninstall bgp-api"
      mapfile -t deployment_images < <(compose images -q 2>/dev/null || true)
      deployment_images+=(
        "$(env_value BGP_API_IMAGE "$ENV_FILE")"
        "$(env_value BGP_API_POSTGRES_IMAGE "$ENV_FILE")"
        "$(env_value BGP_API_SYNC_IMAGE "$ENV_FILE")"
      )
      compose down --volumes --remove-orphans
      for image in "${deployment_images[@]}"; do
        [ -n "$image" ] || continue
        if docker image inspect "$image" >/dev/null 2>&1; then
          docker image rm "$image" >/dev/null 2>&1 || \
            say "warning: kept image still used by another container: $image"
        fi
      done
    else
      say "warning: Docker is unavailable; removing configuration files only"
    fi
  else
    command -v systemctl >/dev/null 2>&1 || die "systemd is required to remove the bare deployment"
    systemctl disable --now bgp-api.service >/dev/null 2>&1 || true
    systemctl disable --now bgp-api-postgres-sync.timer >/dev/null 2>&1 || true
    systemctl stop bgp-api-postgres-sync.service >/dev/null 2>&1 || true
    if command -v runuser >/dev/null 2>&1 && id -u postgres >/dev/null 2>&1; then
      postgres_admin --dbname postgres --command \
        "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = 'bgp_api' AND pid <> pg_backend_pid();" >/dev/null || true
      postgres_admin --dbname postgres --command 'DROP DATABASE IF EXISTS bgp_api;' >/dev/null
      postgres_admin --dbname postgres --command 'DROP ROLE IF EXISTS bgp_api;' >/dev/null
    else
      say "warning: PostgreSQL tools are unavailable; the bgp_api database was not removed"
    fi
    rm -f -- /usr/local/bin/bgp-api "$BARE_ENV_FILE" "$BARE_SERVICE_FILE" \
      /etc/systemd/system/bgp-api-postgres-sync.service \
      /etc/systemd/system/bgp-api-postgres-sync.timer
    rm -rf -- /var/lib/bgp-api
    if id -u bgpapi >/dev/null 2>&1; then
      userdel bgpapi >/dev/null 2>&1 || true
    fi
  fi

  step "Removing the updater and Caddy configuration"
  if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now bgp-api-docker-sync.timer >/dev/null 2>&1 || true
    systemctl stop bgp-api-docker-sync.service >/dev/null 2>&1 || true
  fi
  rm -f -- "$CRON_FILE" /var/log/mehrnet-bgp-api-update.log
  rm -f -- /etc/systemd/system/bgp-api-docker-sync.service \
    /etc/systemd/system/bgp-api-docker-sync.timer
  if command -v crontab >/dev/null 2>&1 && root_crontab="$(crontab -l 2>/dev/null)"; then
    filtered_crontab="$(awk -v script="$INSTALLED_SCRIPT" 'index($0, script " --update") == 0' <<<"$root_crontab")"
    if [ "$filtered_crontab" != "$root_crontab" ]; then
      printf '%s\n' "$filtered_crontab" | crontab -
    fi
  fi
  caddy_changed=false
  if [ -e "$managed_caddy" ]; then
    rm -f -- "$managed_caddy"
    caddy_changed=true
  elif command -v cmp >/dev/null 2>&1 && [ -f "$INSTALL_DIR/deploy/Caddyfile" ] && \
      [ -f "$caddy_main" ] && cmp -s "$INSTALL_DIR/deploy/Caddyfile" "$caddy_main"; then
    : > "$caddy_main"
    caddy_changed=true
  fi
  rm -f -- /etc/bgp-api/caddy.env /etc/systemd/system/caddy.service.d/mehrnet-bgp-api.conf \
    /etc/systemd/system/caddy.service.d/bgp-api.conf
  if [ "$DEPLOY_MODE" = docker ]; then
    rm -f -- "$BARE_ENV_FILE"
  fi
  rmdir /etc/bgp-api /etc/systemd/system/caddy.service.d 2>/dev/null || true
  if [ "$caddy_changed" = true ] && [ -f "$caddy_main" ]; then
    shopt -s nullglob
    remaining_caddy=(/etc/caddy/conf.d/*.caddy)
    shopt -u nullglob
    if [ "${#remaining_caddy[@]}" -eq 0 ]; then
      sed -i '\|^import /etc/caddy/conf.d/\*\.caddy$|d' "$caddy_main"
    fi
  fi
  if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload
    if [ "$caddy_changed" = true ] && systemctl is-active --quiet caddy; then
      if caddy validate --config "$caddy_main" --adapter caddyfile >/dev/null; then
        systemctl reload caddy
      else
        say "warning: Caddy was not reloaded because its remaining configuration is invalid"
      fi
    fi
  fi

  rm -rf -- "$INSTALL_DIR"
  success "MehrNet BGP API was completely removed."
  say "Docker, PostgreSQL, and Caddy packages remain installed for other applications."
}

if [ "$ACTION" = uninstall ]; then
  command -v flock >/dev/null 2>&1 || die "flock is required to uninstall bgp-api safely"
else
  install_base_tools
fi
mkdir -p /run/lock
exec 9>"$LOCK_FILE"
flock -n 9 || die "another install, update, or uninstall is already running"

if [ "$ACTION" = uninstall ]; then
  uninstall_deployment
  exit 0
fi

if [ "$ACTION" = update ]; then
  resolve_installed_mode
  step "Checking for a newer BGP dataset ($DEPLOY_MODE mode)"
  if [ "$DEPLOY_MODE" = docker ]; then
    [ -x "$SYNC_SCRIPT" ] || die "missing $SYNC_SCRIPT"
    [ -f "$ENV_FILE" ] || die "missing $ENV_FILE"
    install_docker
    sync_repository="${BGP_API_GITHUB_REPOSITORY:-$(env_value BGP_API_GITHUB_REPOSITORY "$ENV_FILE")}"
    sync_github_token="${BGP_API_GITHUB_TOKEN:-$(env_value BGP_API_GITHUB_TOKEN "$ENV_FILE")}"
    BGP_API_GITHUB_REPOSITORY="$sync_repository" \
      BGP_API_GITHUB_TOKEN="$sync_github_token" \
      "$SYNC_SCRIPT"
  else
    [ -x "$BARE_SYNC_SCRIPT" ] || die "missing $BARE_SYNC_SCRIPT"
    [ -f "$BARE_ENV_FILE" ] || die "missing $BARE_ENV_FILE"
    install_postgresql
    database_url="$(env_value DATABASE_URL "$BARE_ENV_FILE")"
    sync_repository="${BGP_API_GITHUB_REPOSITORY:-$(env_value BGP_API_GITHUB_REPOSITORY "$BARE_ENV_FILE")}"
    sync_github_token="${BGP_API_GITHUB_TOKEN:-$(env_value BGP_API_GITHUB_TOKEN "$BARE_ENV_FILE")}"
    DATABASE_URL="$database_url" \
      BGP_API_DATABASE_ROLE=bgp_api \
      BGP_API_GITHUB_REPOSITORY="$sync_repository" \
      BGP_API_GITHUB_TOKEN="$sync_github_token" \
      BGP_API_SYNC_MODE=patch \
      BGP_API_BINARY_PATH=/usr/local/bin/bgp-api \
      BGP_API_SERVICE_NAME=bgp-api \
      "$BARE_SYNC_SCRIPT"
  fi
  success "MehrNet BGP API is up to date."
  exit 0
fi

if [ -e "$CONFIG_FILE" ] || [ -e "$ENV_FILE" ] || [ -e "$BARE_ENV_FILE" ]; then
  die "bgp-api is already installed in $INSTALL_DIR; use $INSTALLED_SCRIPT --update"
fi
if [ "$DEPLOY_MODE" = bare ]; then
  install_bare_deployment
  exit 0
fi

case "$(uname -m)" in
  x86_64|amd64) ;;
  *) die "the pre-indexed PostgreSQL image currently requires a Linux amd64 host" ;;
esac

install_docker
if [ "${BGP_API_SKIP_DISK_CHECK:-0}" != 1 ]; then
  docker_root="$(docker info --format '{{.DockerRootDir}}')"
  available_kib="$(df -Pk "$docker_root" | awk 'NR == 2 { print $4 }')"
  required_kib=$((12 * 1024 * 1024))
  [ "$available_kib" -ge "$required_kib" ] || \
    die "Docker needs at least 12 GiB free for the pre-indexed database image"
fi

step "Resolving the latest verified release"
work_dir="$(mktemp -d)"
cleanup() { rm -rf -- "$work_dir"; }
trap cleanup EXIT
release_json="$(github_api "/repos/$REPOSITORY/releases/latest")"
release_tag="$(jq -r '.tag_name // empty' <<<"$release_json")"
[ -n "$release_tag" ] || die "GitHub did not return a published release"
checksums_url="$(jq -r '.assets[] | select(.name == "SHA256SUMS.txt") | .browser_download_url' <<<"$release_json")"
manifest_url="$(jq -r '.assets[] | select(.name == "docker-deployment-manifest.json") | .browser_download_url' <<<"$release_json")"
[ -n "$checksums_url" ] && [ "$checksums_url" != null ] || die "release $release_tag has no SHA256SUMS.txt"
[ -n "$manifest_url" ] && [ "$manifest_url" != null ] || die "release $release_tag has no deployment manifest"
download "$checksums_url" "$work_dir/SHA256SUMS.txt"
download "$manifest_url" "$work_dir/docker-deployment-manifest.json"
expected="$(awk '$2 == "docker-deployment-manifest.json" { print; exit }' "$work_dir/SHA256SUMS.txt")"
[ -n "$expected" ] || die "the checksum manifest does not cover docker-deployment-manifest.json"
(cd "$work_dir" && printf '%s\n' "$expected" | sha256sum -c -)

manifest="$work_dir/docker-deployment-manifest.json"
[ "$(jq -r '.release // empty' "$manifest")" = "$release_tag" ] || die "deployment manifest release mismatch"
api_image="$(jq -r '.images.api // empty' "$manifest")"
postgres_image="$(jq -r '.images.postgres // empty' "$manifest")"
sync_image="$(jq -r '.images.sync // empty' "$manifest")"
repository_owner="${REPOSITORY%%/*}"
legacy_api_prefix="ghcr.io/$REPOSITORY/bgp-api@sha256:"
if [[ "$api_image" == "$legacy_api_prefix"* ]]; then
  api_image="ghcr.io/$repository_owner/bgp-api@sha256:${api_image#"$legacy_api_prefix"}"
fi
valid_pinned_image() {
  local image="$1" repository="$2" digest
  [[ "$image" == "$repository@sha256:"* ]] || return 1
  digest="${image#"$repository@sha256:"}"
  [[ "$digest" =~ ^[0-9a-f]{64}$ ]]
}
valid_pinned_image "$api_image" "ghcr.io/$repository_owner/bgp-api" || die "invalid API image digest"
valid_pinned_image "$postgres_image" "ghcr.io/$repository_owner/bgp-api-postgres" || die "invalid PostgreSQL image digest"
valid_pinned_image "$sync_image" "ghcr.io/$repository_owner/bgp-api-sync" || die "invalid sync image digest"

step "Installing deployment files in $INSTALL_DIR"
source_ref="${BGP_API_SOURCE_REF:-main}"
source_sha="$(github_api "/repos/$REPOSITORY/commits/$source_ref" | jq -r '.sha // empty')"
[[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || die "could not resolve repository source ref $source_ref"
mkdir -p "$INSTALL_DIR/scripts"
download "https://raw.githubusercontent.com/$REPOSITORY/$source_sha/docker-compose.yml" "$work_dir/docker-compose.yml"
download "https://raw.githubusercontent.com/$REPOSITORY/$source_sha/scripts/sync-docker.sh" "$work_dir/sync-docker.sh"
download "https://raw.githubusercontent.com/$REPOSITORY/$source_sha/install.sh" "$work_dir/install.sh"
install -m 0644 "$work_dir/docker-compose.yml" "$COMPOSE_FILE"
install -m 0755 "$work_dir/sync-docker.sh" "$SYNC_SCRIPT"
install -m 0755 "$work_dir/install.sh" "$INSTALLED_SCRIPT"

origin_token=""
trusted_proxies='127.0.0.1/32,::1/128'
if [ "$DOMAIN_SET" = true ]; then
  origin_token="$(openssl rand -hex 32)"
  trusted_proxies='127.0.0.1/32,::1/128,172.16.0.0/12'
fi
cat > "$ENV_FILE" <<EOF
BGP_API_IMAGE=$api_image
BGP_API_POSTGRES_IMAGE=$postgres_image
BGP_API_SYNC_IMAGE=$sync_image
ORIGIN_AUTH_TOKEN=$origin_token
TRUSTED_PROXY_CIDRS=$trusted_proxies
CORS_ALLOWED_ORIGINS_JSON=["https://bgp.mehrnet.com"]
BGP_API_GITHUB_REPOSITORY=$REPOSITORY
BGP_API_GITHUB_TOKEN=${BGP_API_GITHUB_TOKEN:-}
EOF
chmod 0600 "$ENV_FILE"
{
  printf 'MODE=docker\n'
  printf 'DOMAIN=%s\n' "$DOMAIN"
} > "$CONFIG_FILE"
chmod 0600 "$CONFIG_FILE"

step "Pulling the pre-indexed database and API images"
compose pull postgres api sync
compose up -d postgres api
wait_for_api "$origin_token"

if [ "$DOMAIN_SET" = true ]; then
  configure_caddy "$origin_token"
fi
if [ "$AUTO_UPDATE" = true ]; then
  install_cron
fi

say
success "MehrNet BGP API installation completed."
say "Release: $release_tag"
say "Local health: http://127.0.0.1:3102/v1/health"
if [ "$DOMAIN_SET" = true ]; then
  say "Public health: https://$DOMAIN/v1/health"
  say "Caddy will issue TLS certificates after the domain points to this server."
fi
say "Update: $INSTALLED_SCRIPT --mode docker --update"
say "Logs: docker compose --env-file $ENV_FILE -f $COMPOSE_FILE logs -f api"
if [ "$AUTO_UPDATE" = true ]; then
  say "Automatic updates: daily at 06:00 UTC via $CRON_FILE"
else
  say "Automatic updates are disabled; see README.md for the crontab entry."
fi
