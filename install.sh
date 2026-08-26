#!/usr/bin/env bash
# Install, update, or remove the pre-indexed MehrNet BGP API runtime.
set -Eeuo pipefail

readonly REPOSITORY="${BGP_API_GITHUB_REPOSITORY:-mehrnet/bgp-api}"
readonly SOURCE_REF="${BGP_API_SOURCE_REF:-main}"
readonly INSTALL_DIR="${BGP_API_INSTALL_DIR:-/srv/bgp-api}"
readonly DATA_DIR="${BGP_API_DATA_DIR:-/var/lib/bgp-api}"
readonly CONFIG_DIR="/etc/bgp-api"
readonly CONFIG_FILE="$INSTALL_DIR/install.conf"
readonly ENV_FILE="$CONFIG_DIR/bgp-api.env"
readonly UPDATE_ENV_FILE="$CONFIG_DIR/update.env"
readonly SERVICE_FILE="/etc/systemd/system/bgp-api.service"
readonly SYNC_SERVICE_FILE="/etc/systemd/system/bgp-api-sync.service"
readonly SYNC_SCRIPT="$INSTALL_DIR/scripts/sync-bbolt.sh"
readonly INSTALLED_SCRIPT="$INSTALL_DIR/install.sh"
readonly CRON_FILE="/etc/cron.d/mehrnet-bgp-api-update"
readonly CADDY_CONFIG="/etc/caddy/conf.d/mehrnet-bgp-api.caddy"
readonly CADDY_ENV_FILE="$CONFIG_DIR/caddy.env"
readonly CADDY_DROP_IN="/etc/systemd/system/caddy.service.d/mehrnet-bgp-api.conf"

ACTION=install
MODE=bare
DOMAIN=""
DOMAIN_SET=false
AUTO_UPDATE=false

if [ -t 1 ]; then
  readonly BOLD=$'\033[1m' CYAN=$'\033[36m' GREEN=$'\033[32m' RESET=$'\033[0m'
else
  readonly BOLD="" CYAN="" GREEN="" RESET=""
fi

say() { printf '%s\n' "$*"; }
step() { printf '\n%s%s==>%s %s\n' "$BOLD" "$CYAN" "$RESET" "$*"; }
success() { printf '%s%s%s%s\n' "$GREEN" "$BOLD" "$*" "$RESET"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Install, update, or uninstall MehrNet BGP API.

Usage:
  install.sh [--mode bare] [--domain DOMAIN] [--auto-update]
  install.sh [--mode bare] --update
  install.sh [--mode bare] --uninstall

Options:
  --mode bare     Install the native static binary and pre-indexed bbolt file.
  --domain NAME   Install Caddy and serve HTTPS for this hostname.
  --auto-update   Check for a verified release every day at 06:00 UTC.
  --update        Download and atomically activate the latest release.
  --uninstall     Remove the service, data, update job, and managed Caddy config.
  -h, --help      Show this help.

Environment:
  BGP_API_GITHUB_TOKEN       Optional GitHub token for higher API limits.
  BGP_API_GITHUB_REPOSITORY  Release repository (default: mehrnet/bgp-api).
  BGP_API_SOURCE_REF         Deployment-file ref (default: main).
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --mode)
      [ "$#" -ge 2 ] || die "--mode requires bare"
      MODE="${2,,}"
      shift 2
      ;;
    --domain)
      [ "$#" -ge 2 ] || die "--domain requires a hostname"
      DOMAIN="${2,,}"
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
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (use --help)" ;;
  esac
done

[ "$EUID" -eq 0 ] || die "run this installer as root"
[ "$(uname -s)" = Linux ] || die "this installer requires Linux"
[ "$MODE" = bare ] || die "only --mode bare is supported"
command -v systemctl >/dev/null 2>&1 || die "a systemd-based Linux host is required"
if [ "$ACTION" != install ] && { [ "$DOMAIN_SET" = true ] || [ "$AUTO_UPDATE" = true ]; }; then
  die "--$ACTION cannot be combined with installation options"
fi
if [ "$DOMAIN_SET" = true ] && ! [[ "$DOMAIN" =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$ ]]; then
  die "invalid domain: $DOMAIN"
fi

package_manager() {
  for manager in apt-get dnf yum pacman zypper apk; do
    command -v "$manager" >/dev/null 2>&1 && { printf '%s\n' "$manager"; return; }
  done
  return 1
}

install_base_tools() {
  local command manager missing=false
  for command in awk curl date flock jq openssl sha256sum tar zstd; do
    command -v "$command" >/dev/null 2>&1 || missing=true
  done
  [ "$missing" = false ] && return
  manager="$(package_manager)" || die "install curl, jq, openssl, coreutils, tar, util-linux, and zstd, then retry"
  step "Installing required command-line tools"
  case "$manager" in
    apt-get)
      apt-get update
      DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl jq openssl coreutils tar util-linux zstd
      ;;
    dnf|yum) "$manager" install -y ca-certificates curl jq openssl coreutils tar util-linux zstd ;;
    pacman) pacman -Sy --noconfirm ca-certificates curl jq openssl coreutils tar util-linux zstd ;;
    zypper) zypper --non-interactive install ca-certificates curl jq openssl coreutils tar util-linux zstd ;;
    apk) apk add --no-cache ca-certificates curl jq openssl coreutils tar util-linux zstd ;;
  esac
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

configure_caddy() {
  local source_dir="$1" token="$2" main_config=/etc/caddy/Caddyfile
  install_caddy_package
  install -d -m 0755 /etc/caddy/conf.d /etc/systemd/system/caddy.service.d
  [ -e "$main_config" ] || install -m 0644 /dev/null "$main_config"
  install -m 0644 "$source_dir/Caddyfile" "$CADDY_CONFIG"
  grep -Fqx 'import /etc/caddy/conf.d/*.caddy' "$main_config" || \
    printf '\nimport /etc/caddy/conf.d/*.caddy\n' >> "$main_config"
  {
    printf 'BGP_API_DOMAIN=%s\n' "$DOMAIN"
    printf 'ORIGIN_AUTH_TOKEN=%s\n' "$token"
  } > "$CADDY_ENV_FILE"
  chmod 0640 "$CADDY_ENV_FILE"
  if getent group caddy >/dev/null 2>&1; then chown root:caddy "$CADDY_ENV_FILE"; fi
  install -m 0644 "$source_dir/caddy.service.d.env.conf" "$CADDY_DROP_IN"
  caddy fmt --overwrite "$CADDY_CONFIG"
  BGP_API_DOMAIN="$DOMAIN" ORIGIN_AUTH_TOKEN="$token" \
    caddy validate --config "$main_config" --adapter caddyfile
  systemctl daemon-reload
  systemctl enable --now caddy >/dev/null
  systemctl reload caddy
}

install_auto_update() {
  local manager
  if ! command -v cron >/dev/null 2>&1 && ! command -v crond >/dev/null 2>&1; then
    manager="$(package_manager)" || die "a cron daemon is required for --auto-update"
    case "$manager" in
      apt-get) DEBIAN_FRONTEND=noninteractive apt-get install -y cron ;;
      dnf|yum) "$manager" install -y cronie ;;
      pacman) pacman -Sy --noconfirm cronie ;;
      zypper) zypper --non-interactive install cron ;;
      apk) apk add --no-cache dcron ;;
    esac
  fi
  cat > "$CRON_FILE" <<'EOF'
SHELL=/bin/sh
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
CRON_TZ=UTC
0 6 * * * root /usr/bin/systemctl start bgp-api-sync.service
EOF
  chmod 0644 "$CRON_FILE"
  for service in cron crond; do
    if systemctl list-unit-files "$service.service" --no-legend 2>/dev/null | grep -q "^$service.service"; then
      systemctl enable --now "$service.service" >/dev/null
      break
    fi
  done
}

uninstall_deployment() {
  step "Stopping and removing MehrNet BGP API"
  systemctl disable --now bgp-api.service >/dev/null 2>&1 || true
  systemctl stop bgp-api-sync.service >/dev/null 2>&1 || true
  rm -f -- "$SERVICE_FILE" "$SYNC_SERVICE_FILE" "$CRON_FILE" /usr/local/bin/bgp-api
  rm -rf -- "$DATA_DIR" "$INSTALL_DIR"
  rm -f -- "$ENV_FILE" "$UPDATE_ENV_FILE" "$CADDY_ENV_FILE" "$CADDY_CONFIG" "$CADDY_DROP_IN"
  rmdir "$CONFIG_DIR" /etc/systemd/system/caddy.service.d /etc/caddy/conf.d 2>/dev/null || true
  systemctl daemon-reload
  if command -v caddy >/dev/null 2>&1 && systemctl is-active --quiet caddy; then
    caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null && systemctl reload caddy
  fi
  if id -u bgpapi >/dev/null 2>&1; then userdel bgpapi >/dev/null 2>&1 || true; fi
  success "MehrNet BGP API was removed."
}

if [ "$ACTION" = uninstall ]; then
  uninstall_deployment
  exit 0
fi

if [ "$ACTION" = update ]; then
  [ -x "$SYNC_SCRIPT" ] || die "no bbolt installation found at $INSTALL_DIR"
  exec "$SYNC_SCRIPT"
fi

install_base_tools
case "$(uname -m)" in
  x86_64|amd64|aarch64|arm64) ;;
  *) die "unsupported CPU architecture: $(uname -m)" ;;
esac

step "Preparing the native bbolt service"
if ! id -u bgpapi >/dev/null 2>&1; then
  useradd --system --home-dir "$DATA_DIR" --create-home --shell /usr/sbin/nologin bgpapi
fi
install -d -m 0750 -o bgpapi -g bgpapi "$DATA_DIR"
install -d -m 0755 "$INSTALL_DIR/scripts" "$CONFIG_DIR"

source_sha="$(github_api "/repos/$REPOSITORY/commits/$SOURCE_REF" | jq -er '.sha')"
[[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || die "could not resolve repository source ref $SOURCE_REF"
work_dir="$(mktemp -d)"
trap 'rm -rf -- "$work_dir"' EXIT
raw="https://raw.githubusercontent.com/$REPOSITORY/$source_sha"
download "$raw/install.sh" "$work_dir/install.sh"
download "$raw/scripts/sync-bbolt.sh" "$work_dir/sync-bbolt.sh"
download "$raw/deploy/bgp-api.service" "$work_dir/bgp-api.service"
download "$raw/deploy/bgp-api-sync.service" "$work_dir/bgp-api-sync.service"
download "$raw/deploy/Caddyfile" "$work_dir/Caddyfile"
download "$raw/deploy/caddy.service.d.env.conf" "$work_dir/caddy.service.d.env.conf"
install -m 0755 "$work_dir/install.sh" "$INSTALLED_SCRIPT"
install -m 0755 "$work_dir/sync-bbolt.sh" "$SYNC_SCRIPT"
install -m 0644 "$work_dir/bgp-api.service" "$SERVICE_FILE"
install -m 0644 "$work_dir/bgp-api-sync.service" "$SYNC_SERVICE_FILE"

origin_token=""
if [ "$DOMAIN_SET" = true ]; then origin_token="$(openssl rand -hex 32)"; fi
cat > "$ENV_FILE" <<EOF
BGP_API_DATABASE_PATH=$DATA_DIR/mehrnet_bgp.bbolt
LISTEN_ADDR=127.0.0.1:3102
GOMAXPROCS=2
GOMEMLIMIT=384MiB
ORIGIN_AUTH_TOKEN=$origin_token
TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128
CORS_ALLOWED_ORIGINS_JSON='["https://bgp.mehrnet.com"]'
EOF
chown root:bgpapi "$ENV_FILE"
chmod 0640 "$ENV_FILE"
cat > "$UPDATE_ENV_FILE" <<EOF
BGP_API_GITHUB_REPOSITORY=$REPOSITORY
BGP_API_GITHUB_TOKEN=${BGP_API_GITHUB_TOKEN:-}
EOF
chmod 0600 "$UPDATE_ENV_FILE"
{
  printf 'MODE=bare\n'
  printf 'DOMAIN=%s\n' "$DOMAIN"
} > "$CONFIG_FILE"
chmod 0600 "$CONFIG_FILE"

systemctl daemon-reload
systemctl enable bgp-api.service >/dev/null
step "Downloading and activating the latest verified release"
"$SYNC_SCRIPT"

if [ "$DOMAIN_SET" = true ]; then
  step "Configuring HTTPS for $DOMAIN"
  configure_caddy "$work_dir" "$origin_token"
fi
if [ "$AUTO_UPDATE" = true ]; then
  step "Scheduling daily updates at 06:00 UTC"
  install_auto_update
fi

release_tag="$(tr -d '[:space:]' < "$DATA_DIR/release-tag")"
trap - EXIT
rm -rf -- "$work_dir"
success "MehrNet BGP API installation completed."
say "Release: $release_tag"
say "Local API: http://127.0.0.1:3102"
[ "$DOMAIN_SET" = false ] || say "Public API: https://$DOMAIN"
say "Update: sudo $INSTALLED_SCRIPT --update"
say "Logs: journalctl -u bgp-api -f"
say "Sync logs: journalctl -u bgp-api-sync"
[ "$AUTO_UPDATE" = false ] || say "Automatic updates: daily at 06:00 UTC"
