#!/usr/bin/env bash
# Download, verify, validate, and activate the latest immutable bbolt release.
# When Caddy is active, updates use a temporary secondary API slot and a
# graceful proxy reload. Hosts without a proxy (or without enough disk space)
# use the same-port stop-and-replace fallback.
set -Eeuo pipefail

readonly REPOSITORY="${BGP_API_GITHUB_REPOSITORY:-mehrnet/bgp-api}"
readonly DATA_DIR="${BGP_API_DATA_DIR:-/var/lib/bgp-api}"
readonly PRIMARY_DATABASE_PATH="${BGP_API_PRIMARY_DATABASE_PATH:-${BGP_API_DATABASE_PATH:-$DATA_DIR/primary.bbolt}}"
readonly SECONDARY_DATABASE_PATH="${BGP_API_SECONDARY_DATABASE_PATH:-$DATA_DIR/secondary.bbolt}"
readonly LEGACY_DATABASE_PATH="$DATA_DIR/mehrnet_bgp.bbolt"
readonly BINARY_PATH="${BGP_API_BINARY_PATH:-/usr/local/bin/bgp-api}"
readonly RELEASE_FILE="${BGP_API_RELEASE_FILE:-$DATA_DIR/release-tag}"
readonly PUBLISHED_FILE="${BGP_API_PUBLISHED_FILE:-$DATA_DIR/release-published-at}"
readonly ACTIVE_SLOT_FILE="${BGP_API_ACTIVE_SLOT_FILE:-$DATA_DIR/active-slot}"
readonly SERVICE="${BGP_API_SERVICE:-bgp-api.service}"
readonly SECONDARY_SERVICE="${BGP_API_SECONDARY_SERVICE:-bgp-api-secondary.service}"
readonly PRIMARY_ENV_FILE="${BGP_API_PRIMARY_ENV_FILE:-/etc/bgp-api/bgp-api.env}"
readonly SECONDARY_ENV_FILE="${BGP_API_SECONDARY_ENV_FILE:-/etc/bgp-api/bgp-api-secondary.env}"
readonly CADDY_ENV_FILE="${BGP_API_CADDY_ENV_FILE:-/etc/bgp-api/caddy.env}"
readonly CADDY_CONFIG="${BGP_API_CADDY_CONFIG:-/etc/caddy/Caddyfile}"
readonly CADDY_MANAGED_CONFIG="${BGP_API_CADDY_MANAGED_CONFIG:-/etc/caddy/conf.d/mehrnet-bgp-api.caddy}"
readonly LOCK_FILE="${BGP_API_LOCK_FILE:-/run/lock/mehrnet-bgp-api-sync.lock}"
readonly BLUE_GREEN_MODE="${BGP_API_BLUE_GREEN:-auto}"
readonly STARTUP_TIMEOUT_SECONDS="${BGP_API_STARTUP_TIMEOUT_SECONDS:-300}"
readonly STAGE_MEMORY_RESERVE_MIB="${BGP_API_STAGE_MEMORY_RESERVE_MIB:-768}"
readonly DRAIN_GRACE_SECONDS="${BGP_API_DRAIN_GRACE_SECONDS:-45}"

say() { printf '%s %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*"; }
die() { say "error: $*" >&2; exit 1; }

# Dataset transfer and extraction are deliberately background-priority work.
# The active API is latency-sensitive and must retain CPU and block I/O while
# a multi-gigabyte immutable release is prepared in the inactive slot.
low_priority() {
  if command -v ionice >/dev/null 2>&1; then
    ionice -c3 nice -n 19 "$@"
    return
  fi
  nice -n 19 "$@"
}

[[ "$STARTUP_TIMEOUT_SECONDS" =~ ^[1-9][0-9]*$ ]] || die "BGP_API_STARTUP_TIMEOUT_SECONDS must be a positive integer"
[[ "$STAGE_MEMORY_RESERVE_MIB" =~ ^[1-9][0-9]*$ ]] || die "BGP_API_STAGE_MEMORY_RESERVE_MIB must be a positive integer"
[[ "$DRAIN_GRACE_SECONDS" =~ ^[1-9][0-9]*$ ]] || die "BGP_API_DRAIN_GRACE_SECONDS must be a positive integer"

for command in awk cat chown cp curl date df flock grep head install jq mktemp mv rm sed sha256sum sleep stat systemctl tar tr zstd; do
  command -v "$command" >/dev/null 2>&1 || die "missing required command: $command"
done

install -d -m 0750 -o bgpapi -g bgpapi "$DATA_DIR"
install -d -m 0755 "$(dirname "$LOCK_FILE")"
exec 9>"$LOCK_FILE"
flock -n 9 || { say "another sync is already running"; exit 0; }

started_at="$(date +%s)"
sync_tmp_root="${BGP_API_SYNC_TMPDIR:-${TMPDIR:-/tmp}}"
work_dir="$(mktemp -d "$sync_tmp_root/bgp-api-sync.XXXXXX")"
trap 'rm -rf -- "$work_dir"' EXIT
active_caches_released=false

github_api() {
  local -a headers=(-H 'Accept: application/vnd.github+json')
  if [ -n "${BGP_API_GITHUB_TOKEN:-}" ]; then
    headers+=(-H "Authorization: Bearer $BGP_API_GITHUB_TOKEN")
  fi
  low_priority curl --fail --location --silent --show-error --retry 3 \
    "${headers[@]}" "https://api.github.com$1"
}

download() {
  local -a headers=()
  if [ -n "${BGP_API_GITHUB_TOKEN:-}" ]; then
    headers+=(-H "Authorization: Bearer $BGP_API_GITHUB_TOKEN")
  fi
  low_priority curl --fail --location --silent --show-error --retry 3 \
    "${headers[@]}" --output "$2" "$1"
}

asset_url() {
  jq -er --arg name "$1" '.assets[] | select(.name == $name) | .browser_download_url' <<<"$release_json"
}

verify_asset() {
  local asset="$1" expected
  expected="$(awk -v name="$asset" '$2 == name { print $1; exit }' "$work_dir/SHA256SUMS.txt")"
  [ -n "$expected" ] || die "SHA256SUMS.txt does not cover $asset"
  (cd "$work_dir" && printf '%s  %s\n' "$expected" "$asset" | sha256sum --check --status -) || \
    die "checksum verification failed for $asset"
}

env_value() {
  local name="$1" path="$2"
  [ -f "$path" ] || return 0
  sed -n "s/^${name}=//p" "$path" | head -n 1
}

service_active() { systemctl is-active --quiet "$1"; }

memory_available_mib() {
  awk '/^MemAvailable:/ { printf "%d\n", $2 / 1024; exit }' /proc/meminfo
}

service_main_pid() {
  local service="$1" pid
  pid="$(systemctl show --property MainPID --value "$service")"
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
  printf '%s\n' "$pid"
}

process_rss_mib() {
  local pid="$1"
  awk '/^VmRSS:/ { printf "%d\n", $2 / 1024; exit }' "/proc/$pid/status" 2>/dev/null
}

runtime_cache_control_supported() {
  local service="$1" port="$2" token response
  service_main_pid "$service" >/dev/null || return 1
  token="$(env_value ORIGIN_AUTH_TOKEN "$CADDY_ENV_FILE")"
  response="$(curl --fail --silent --show-error --max-time 3 \
    -H "X-BGP-API-Origin-Token: $token" \
    "http://127.0.0.1:$port/v1/health" 2>/dev/null)" || return 1
  jq -e '.runtime.cache_control == true' <<<"$response" >/dev/null
}

release_active_caches_before_stage() {
  local service="$1" port="$2" available_before available_after pid rss_before rss_after attempt
  available_before="$(memory_available_mib)"
  if [ "$available_before" -ge "$STAGE_MEMORY_RESERVE_MIB" ]; then
    pid="$(service_main_pid "$service" 2>/dev/null || true)"
    rss_before="${pid:+$(process_rss_mib "$pid" 2>/dev/null || true)}"
    say "memory before staging: available=${available_before} MiB active_rss=${rss_before:-unknown} MiB; retaining active runtime caches"
    return
  fi
  if ! runtime_cache_control_supported "$service" "$port"; then
    say "memory before staging: available=${available_before} MiB is below ${STAGE_MEMORY_RESERVE_MIB} MiB, but $service does not support runtime cache release; staging without it"
    return
  fi
  pid="$(service_main_pid "$service")"
  rss_before="$(process_rss_mib "$pid" 2>/dev/null || true)"
  say "memory before staging: available=${available_before} MiB active_rss=${rss_before:-unknown} MiB; asking $service to release runtime caches"
  if ! systemctl kill --signal=USR2 "$service"; then
    say "could not signal $service to release runtime caches; staging without it"
    return
  fi
  active_caches_released=true
  for ((attempt = 0; attempt < 10; attempt++)); do
    sleep 1
    available_after="$(memory_available_mib)"
    [ "$available_after" -ge "$STAGE_MEMORY_RESERVE_MIB" ] && break
  done
  rss_after="$(process_rss_mib "$pid" 2>/dev/null || true)"
  say "memory after cache release: available=${available_after:-unknown} MiB active_rss=${rss_after:-unknown} MiB"
}

restore_active_caches_after_failed_stage() {
  local service="$1"
  [ "$active_caches_released" = true ] || return
  say "restoring runtime caches in $service after the staged update did not activate"
  if ! systemctl kill --signal=USR1 "$service"; then
    say "could not signal $service to restore runtime caches"
  fi
  active_caches_released=false
}

stop_service() {
  systemctl stop "$1" >/dev/null 2>&1 || true
}

wait_stopped() {
  local service="$1" attempt
  for ((attempt = 0; attempt < 30; attempt++)); do
    service_active "$service" || return 0
    sleep 1
  done
  return 1
}

service_for_slot() {
  case "$1" in
    primary) printf '%s\n' "$SERVICE" ;;
    secondary) printf '%s\n' "$SECONDARY_SERVICE" ;;
    *) return 1 ;;
  esac
}

database_for_slot() {
  case "$1" in
    primary) printf '%s\n' "$PRIMARY_DATABASE_PATH" ;;
    secondary) printf '%s\n' "$SECONDARY_DATABASE_PATH" ;;
    *) return 1 ;;
  esac
}

port_for_slot() {
  case "$1" in
    primary) printf '3102\n' ;;
    secondary) printf '3103\n' ;;
    *) return 1 ;;
  esac
}

env_file_for_slot() {
  case "$1" in
    primary) printf '%s\n' "$PRIMARY_ENV_FILE" ;;
    secondary) printf '%s\n' "$SECONDARY_ENV_FILE" ;;
    *) return 1 ;;
  esac
}

set_env_value() {
  local name="$1" value="$2" path="$3" temporary owner group mode
  owner="$(stat -c '%u' "$path")"
  group="$(stat -c '%g' "$path")"
  mode="$(stat -c '%a' "$path")"
  temporary="$(mktemp "${path}.tmp.XXXXXX")"
  if grep -q "^${name}=" "$path"; then
    sed "s|^${name}=.*|${name}=${value}|" "$path" > "$temporary"
  else
    cat "$path" > "$temporary"
    printf '%s=%s\n' "$name" "$value" >> "$temporary"
  fi
  chown "$owner:$group" "$temporary"
  chmod "$mode" "$temporary"
  mv -f "$temporary" "$path"
}

read_active_slot() {
  local slot=""
  local upstream
  upstream="$(env_value BGP_API_UPSTREAM "$CADDY_ENV_FILE")"
  case "$upstream" in
    127.0.0.1:3102)
      if service_active "$SERVICE"; then
        printf 'primary\n'
        return
      fi
      ;;
    127.0.0.1:3103)
      if service_active "$SECONDARY_SERVICE"; then
        printf 'secondary\n'
        return
      fi
      ;;
  esac
  if [ -s "$ACTIVE_SLOT_FILE" ]; then
    slot="$(tr -d '[:space:]' < "$ACTIVE_SLOT_FILE")"
    case "$slot" in
      primary|secondary) printf '%s\n' "$slot"; return ;;
    esac
  fi
  if service_active "$SERVICE"; then
    printf 'primary\n'
  elif service_active "$SECONDARY_SERVICE"; then
    printf 'secondary\n'
  elif [ -s "$PRIMARY_DATABASE_PATH" ]; then
    printf 'primary\n'
  else
    printf 'none\n'
  fi
}

write_active_slot() {
  local slot="$1" temporary="${ACTIVE_SLOT_FILE}.tmp.$$"
  printf '%s\n' "$slot" > "$temporary"
  chown bgpapi:bgpapi "$temporary"
  chmod 0640 "$temporary"
  mv -f "$temporary" "$ACTIVE_SLOT_FILE"
}

blue_green_available() {
  case "${BLUE_GREEN_MODE,,}" in
    0|false|no|off) return 1 ;;
    1|true|yes|on) ;;
    auto)
      command -v caddy >/dev/null 2>&1 || return 1
      [ -s "$CADDY_ENV_FILE" ] || return 1
      [ -s "$CADDY_CONFIG" ] || return 1
      grep -Fq 'BGP_API_UPSTREAM' "$CADDY_MANAGED_CONFIG" || return 1
      service_active caddy || return 1
      ;;
    *) die "BGP_API_BLUE_GREEN must be auto, 0, or 1" ;;
  esac
  command -v caddy >/dev/null 2>&1 || die "blue-green updates require caddy"
  [ -s "$CADDY_ENV_FILE" ] || die "blue-green updates require $CADDY_ENV_FILE"
  [ -s "$CADDY_CONFIG" ] || die "blue-green updates require $CADDY_CONFIG"
  grep -Fq 'BGP_API_UPSTREAM' "$CADDY_MANAGED_CONFIG" || die "Caddyfile does not support blue-green upstream switching"
  service_active caddy || die "blue-green updates require an active caddy service"
  return 0
}

caddy_configured() {
  command -v caddy >/dev/null 2>&1 &&
    [ -s "$CADDY_ENV_FILE" ] &&
    [ -s "$CADDY_CONFIG" ] &&
    grep -Fq 'BGP_API_UPSTREAM' "$CADDY_MANAGED_CONFIG"
}

set_caddy_upstream() {
  local upstream="$1" temporary owner group mode
  owner="$(stat -c '%u' "$CADDY_ENV_FILE")"
  group="$(stat -c '%g' "$CADDY_ENV_FILE")"
  mode="$(stat -c '%a' "$CADDY_ENV_FILE")"
  temporary="$(mktemp "${CADDY_ENV_FILE}.tmp.XXXXXX")"
  if grep -q '^BGP_API_UPSTREAM=' "$CADDY_ENV_FILE"; then
    sed "s|^BGP_API_UPSTREAM=.*|BGP_API_UPSTREAM=$upstream|" "$CADDY_ENV_FILE" > "$temporary"
  else
    cat "$CADDY_ENV_FILE" > "$temporary"
    printf 'BGP_API_UPSTREAM=%s\n' "$upstream" >> "$temporary"
  fi
  chown "$owner:$group" "$temporary"
  chmod "$mode" "$temporary"
  mv -f "$temporary" "$CADDY_ENV_FILE"
}

restore_caddy_upstream() {
  local upstream="$1"
  if [ -n "$upstream" ]; then
    set_caddy_upstream "$upstream"
  else
    local temporary owner group mode
    owner="$(stat -c '%u' "$CADDY_ENV_FILE")"
    group="$(stat -c '%g' "$CADDY_ENV_FILE")"
    mode="$(stat -c '%a' "$CADDY_ENV_FILE")"
    temporary="$(mktemp "${CADDY_ENV_FILE}.tmp.XXXXXX")"
    sed '/^BGP_API_UPSTREAM=/d' "$CADDY_ENV_FILE" > "$temporary"
    chown "$owner:$group" "$temporary"
    chmod "$mode" "$temporary"
    mv -f "$temporary" "$CADDY_ENV_FILE"
  fi
}

reload_caddy() {
  local upstream="$1"
  local domain token
  domain="$(env_value BGP_API_DOMAIN "$CADDY_ENV_FILE")"
  token="$(env_value ORIGIN_AUTH_TOKEN "$CADDY_ENV_FILE")"
  BGP_API_DOMAIN="${domain:-bgp-api.mehrnet.com}" \
    BGP_API_UPSTREAM="$upstream" \
    ORIGIN_AUTH_TOKEN="$token" \
    caddy validate --config "$CADDY_CONFIG" --adapter caddyfile >/dev/null
  BGP_API_DOMAIN="${domain:-bgp-api.mehrnet.com}" \
    BGP_API_UPSTREAM="$upstream" \
    ORIGIN_AUTH_TOKEN="$token" \
    caddy reload --config "$CADDY_CONFIG" --adapter caddyfile >/dev/null
}

switch_caddy() {
  local port="$1" previous
  previous="$(env_value BGP_API_UPSTREAM "$CADDY_ENV_FILE")"
  set_caddy_upstream "127.0.0.1:$port"
  if ! reload_caddy "127.0.0.1:$port"; then
    restore_caddy_upstream "$previous"
    return 1
  fi
}

health_check() {
  local port="$1" token response
  token="$(env_value ORIGIN_AUTH_TOKEN "$CADDY_ENV_FILE")"
  for ((attempt = 0; attempt < STARTUP_TIMEOUT_SECONDS; attempt++)); do
    if response="$(curl --fail --silent --show-error --max-time 3 \
      -H "X-BGP-API-Origin-Token: $token" \
      "http://127.0.0.1:$port/v1/health" 2>/dev/null)" && \
      jq -e --arg release "$release_tag" '.ok == true and .dataset.release_tag == $release' <<<"$response" >/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

restore_binary() {
  local backup="$1"
  if [ -s "$backup" ]; then
    install -o root -g root -m 0755 "$backup" "$BINARY_PATH"
  else
    rm -f -- "$BINARY_PATH"
  fi
}

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported CPU architecture: $(uname -m)" ;;
esac

say "checking GitHub for a published dataset"
release_json="$(github_api "/repos/$REPOSITORY/releases/latest")"
release_tag="$(jq -er '.tag_name' <<<"$release_json")"
published_at="$(jq -er '.published_at' <<<"$release_json")"
published_epoch="$(date -u -d "$published_at" +%s)"
now_epoch="$(date +%s)"
[ "$published_epoch" -le "$((now_epoch + 300))" ] || die "latest release timestamp is in the future"

installed_tag=""
[ ! -f "$RELEASE_FILE" ] || installed_tag="$(tr -d '[:space:]' < "$RELEASE_FILE")"
if [ "$installed_tag" = "$release_tag" ] && [ "${BGP_API_FORCE_SYNC:-0}" != 1 ]; then
  say "release $release_tag is already active"
  exit 0
fi
if [ -s "$PUBLISHED_FILE" ] && [ "${BGP_API_FORCE_SYNC:-0}" != 1 ]; then
  installed_published_at="$(tr -d '[:space:]' < "$PUBLISHED_FILE")"
  installed_published_epoch="$(date -u -d "$installed_published_at" +%s)"
  if [ "$published_epoch" -le "$installed_published_epoch" ]; then
    say "release $release_tag is not newer than the active dataset"
    exit 0
  fi
fi

database_asset=mehrnet_bgp_bbolt.tar.zst
binary_asset="bgp-api-linux-${arch}.tar.gz"
download "$(asset_url SHA256SUMS.txt)" "$work_dir/SHA256SUMS.txt"
download "$(asset_url "$database_asset")" "$work_dir/$database_asset"
download "$(asset_url "$binary_asset")" "$work_dir/$binary_asset"
verify_asset "$database_asset"
verify_asset "$binary_asset"

say "extracting release $release_tag"
mkdir "$work_dir/database" "$work_dir/binary"
low_priority zstd --decompress --stdout "$work_dir/$database_asset" | \
  low_priority tar -xf - -C "$work_dir/database"
low_priority tar -xzf "$work_dir/$binary_asset" -C "$work_dir/binary"
new_database="$work_dir/database/mehrnet_bgp.bbolt"
new_binary="$work_dir/binary/bgp-api-linux-${arch}"
[ -s "$new_database" ] || die "database archive did not contain mehrnet_bgp.bbolt"
[ -s "$new_binary" ] || die "binary archive did not contain bgp-api-linux-${arch}"
chmod 0755 "$new_binary"

say "validating downloaded database"
BGP_API_DATABASE_PATH="$new_database" BGP_API_VALIDATE_ONLY=1 "$new_binary"

active_slot="$(read_active_slot)"
active_service=""
active_database=""
if [ "$active_slot" != none ]; then
  active_service="$(service_for_slot "$active_slot")"
  active_database="$(database_for_slot "$active_slot")"
fi

blue_green=false
if blue_green_available && [ "$active_slot" != none ] && service_active "$active_service"; then
  blue_green=true
fi

target_slot=primary
if [ "$active_slot" = primary ]; then
  target_slot=secondary
elif [ "$active_slot" = secondary ]; then
  target_slot=primary
fi
target_service="$(service_for_slot "$target_slot")"
target_database="$(database_for_slot "$target_slot")"
target_port="$(port_for_slot "$target_slot")"
target_env_file="$(env_file_for_slot "$target_slot")"

new_size="$(stat -c '%s' "$new_database")"
available_bytes="$(df -P -B1 "$DATA_DIR" | awk 'NR == 2 { print $4 }')"
headroom=$((64 * 1024 * 1024))
if [ "$blue_green" = true ]; then
  if service_active "$target_service"; then
    # A crash after a proxy switch can leave both slots running. The active
    # slot was derived from Caddy above, so the other service is safe to drain
    # before its database path is reused.
    say "$target_service is still running from an interrupted update; draining it before reuse"
    stop_service "$target_service"
    wait_stopped "$target_service" || die "$target_service did not stop before its slot was reused"
  fi
  # The inactive slot is disposable. Removing it first makes the capacity
  # check reflect the space actually available for the staged immutable file.
  rm -f -- "$target_database"
  available_bytes="$(df -P -B1 "$DATA_DIR" | awk 'NR == 2 { print $4 }')"
  if [ "$available_bytes" -lt "$((new_size + headroom))" ]; then
    say "not enough free space for a second database; using stop-and-replace"
    blue_green=false
  fi
fi

binary_backup="$work_dir/bgp-api.previous"
if [ -s "$BINARY_PATH" ]; then
  cp "$BINARY_PATH" "$binary_backup"
fi
install -o root -g root -m 0755 "$new_binary" "$BINARY_PATH"

if [ "$blue_green" = true ]; then
  say "staging release $release_tag in the $target_slot API slot"
  mv "$new_database" "$target_database"
  chown bgpapi:bgpapi "$target_database"
  chmod 0440 "$target_database"
  # A compatible old slot can shed optional heap caches when memory is tight.
  # The current bbolt mmap stays valid, so it continues serving correct
  # responses until Caddy moves traffic to the staged slot.
  release_active_caches_before_stage "$active_service" "$(port_for_slot "$active_slot")"
  # A full page-cache warm or selector preload must not run in both API slots
  # at once. systemd reads this file before exec, so restoring it after start
  # affects the next process only; SIGUSR1 starts the active slot's warmup.
  deferred_cache_warmup_before="$(env_value BGP_API_DEFER_CACHE_WARMUP "$target_env_file")"
  # Persist capability state for the slot that will become active. The next
  # update can safely request SIGUSR2 only after it sees this process flag.
  set_env_value BGP_API_RUNTIME_CACHE_CONTROL 1 "$target_env_file"
  set_env_value BGP_API_DEFER_CACHE_WARMUP 1 "$target_env_file"
  if ! systemctl start "$target_service"; then
    set_env_value BGP_API_DEFER_CACHE_WARMUP "${deferred_cache_warmup_before:-0}" "$target_env_file"
    restore_active_caches_after_failed_stage "$active_service"
    die "$target_service could not start"
  fi
  set_env_value BGP_API_DEFER_CACHE_WARMUP "${deferred_cache_warmup_before:-0}" "$target_env_file"
  if ! health_check "$target_port"; then
    stop_service "$target_service"
    if ! wait_stopped "$target_service"; then
      restore_binary "$binary_backup"
      restore_active_caches_after_failed_stage "$active_service"
      die "$target_service did not stop after a failed health check"
    fi
    rm -f -- "$target_database"
    restore_binary "$binary_backup"
    restore_active_caches_after_failed_stage "$active_service"
    die "$target_service did not become healthy; active slot was left unchanged"
  fi
  staged_pid="$(service_main_pid "$target_service" 2>/dev/null || true)"
  staged_rss="${staged_pid:+$(process_rss_mib "$staged_pid" 2>/dev/null || true)}"
  say "memory with both slots running: available=$(memory_available_mib) MiB staged_rss=${staged_rss:-unknown} MiB"
  systemctl enable "$target_service" >/dev/null

  say "switching Caddy traffic to $target_service on 127.0.0.1:$target_port"
  if ! switch_caddy "$target_port"; then
    stop_service "$target_service"
    if ! wait_stopped "$target_service"; then
      restore_binary "$binary_backup"
      restore_active_caches_after_failed_stage "$active_service"
      die "$target_service did not stop after Caddy rejected the new upstream"
    fi
    systemctl disable "$target_service" >/dev/null 2>&1 || true
    rm -f -- "$target_database"
    restore_binary "$binary_backup"
    restore_active_caches_after_failed_stage "$active_service"
    die "Caddy rejected the new upstream; active slot was left unchanged"
  fi
  if ! write_active_slot "$target_slot"; then
    if ! switch_caddy "$(port_for_slot "$active_slot")"; then
      die "could not persist the active slot or restore Caddy; the new slot remains serving"
    fi
    stop_service "$target_service"
    if ! wait_stopped "$target_service"; then
      restore_binary "$binary_backup"
      restore_active_caches_after_failed_stage "$active_service"
      die "$target_service did not stop after the slot marker failed"
    fi
    systemctl disable "$target_service" >/dev/null 2>&1 || true
    rm -f -- "$target_database"
    restore_binary "$binary_backup"
    restore_active_caches_after_failed_stage "$active_service"
    die "could not persist the active slot; active slot was left unchanged"
  fi

  # A Caddy reload is atomic for new connections, but existing HTTP/2 and
  # Cloudflare origin connections may still be handled by the old proxy route
  # briefly. Keep the old backend alive through that grace period before its
  # systemd stop closes those in-flight/stale upstream requests.
  say "allowing $active_service to drain for ${DRAIN_GRACE_SECONDS}s before removal"
  sleep "$DRAIN_GRACE_SECONDS"
  say "draining $active_service before removing its database"
  stop_service "$active_service"
  if ! wait_stopped "$active_service"; then
    if ! switch_caddy "$(port_for_slot "$active_slot")"; then
      say "the new slot remains serving; warming its runtime caches"
      systemctl kill --signal=USR1 "$target_service" || true
      die "$active_service did not drain and Caddy could not restore the old upstream; the new slot remains serving"
    fi
    write_active_slot "$active_slot" || true
    stop_service "$target_service"
    if ! wait_stopped "$target_service"; then
      restore_binary "$binary_backup"
      restore_active_caches_after_failed_stage "$active_service"
      die "$target_service did not stop after the drain failure"
    fi
    systemctl disable "$target_service" >/dev/null 2>&1 || true
    rm -f -- "$target_database"
    restore_binary "$binary_backup"
    restore_active_caches_after_failed_stage "$active_service"
    die "$active_service did not drain within 30 seconds; active slot was left unchanged"
  fi
  rm -f -- "$active_database"
  systemctl disable "$active_service" >/dev/null 2>&1 || true
  write_active_slot "$target_slot"
  say "warming runtime caches in the active $target_service slot"
  systemctl kill --signal=USR1 "$target_service"
else
  say "using stop-and-replace activation for release $release_tag"
  stop_service "$SERVICE"
  stop_service "$SECONDARY_SERVICE"
  wait_stopped "$SERVICE" || die "$SERVICE did not stop"
  wait_stopped "$SECONDARY_SERVICE" || die "$SECONDARY_SERVICE did not stop"
  rm -f -- "$PRIMARY_DATABASE_PATH" "$SECONDARY_DATABASE_PATH" "$LEGACY_DATABASE_PATH"
  mv "$new_database" "$PRIMARY_DATABASE_PATH"
  chown bgpapi:bgpapi "$PRIMARY_DATABASE_PATH"
  chmod 0440 "$PRIMARY_DATABASE_PATH"
  systemctl enable "$SERVICE" >/dev/null
  systemctl disable "$SECONDARY_SERVICE" >/dev/null 2>&1 || true
  systemctl start "$SERVICE"
  if ! health_check 3102; then
    restore_binary "$binary_backup"
    die "$SERVICE did not become healthy after stop-and-replace activation"
  fi
  if caddy_configured; then
    if service_active caddy; then
      switch_caddy 3102 || die "Caddy could not switch to the primary API slot"
    else
      # Persist the next upstream even when Caddy is currently down. Its
      # systemd environment will use this value on the next start.
      set_caddy_upstream 127.0.0.1:3102
      say "Caddy is inactive; persisted the primary upstream for its next start"
    fi
  fi
  write_active_slot primary
fi

printf '%s\n' "$release_tag" > "$RELEASE_FILE"
printf '%s\n' "$published_at" > "$PUBLISHED_FILE"
chown bgpapi:bgpapi "$RELEASE_FILE" "$PUBLISHED_FILE"
chmod 0640 "$RELEASE_FILE" "$PUBLISHED_FILE"
rm -f -- "$binary_backup"
elapsed="$(( $(date +%s) - started_at ))"
if [ "$blue_green" = true ]; then
  mode=blue-green
else
  mode=stop-and-replace
fi
say "release $release_tag is active in $mode mode; sync completed in ${elapsed}s"
