#!/usr/bin/env bash
# Download, verify, validate, and atomically activate the latest bbolt release.
set -Eeuo pipefail

readonly REPOSITORY="${BGP_API_GITHUB_REPOSITORY:-mehrnet/bgp-api}"
readonly DATA_DIR="${BGP_API_DATA_DIR:-/var/lib/bgp-api}"
readonly DATABASE_PATH="${BGP_API_DATABASE_PATH:-$DATA_DIR/mehrnet_bgp.bbolt}"
readonly BINARY_PATH="${BGP_API_BINARY_PATH:-/usr/local/bin/bgp-api}"
readonly RELEASE_FILE="${BGP_API_RELEASE_FILE:-$DATA_DIR/release-tag}"
readonly PUBLISHED_FILE="${BGP_API_PUBLISHED_FILE:-$DATA_DIR/release-published-at}"
readonly SERVICE="${BGP_API_SERVICE:-bgp-api.service}"
readonly LOCK_FILE="${BGP_API_LOCK_FILE:-/run/lock/mehrnet-bgp-api-sync.lock}"

say() { printf '%s %s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" "$*"; }
die() { say "error: $*" >&2; exit 1; }

for command in awk chown curl date flock install jq sha256sum systemctl tar zstd; do
  command -v "$command" >/dev/null 2>&1 || die "missing required command: $command"
done

install -d -m 0750 -o bgpapi -g bgpapi "$DATA_DIR"
install -d -m 0755 "$(dirname "$LOCK_FILE")"
exec 9>"$LOCK_FILE"
flock -n 9 || { say "another sync is already running"; exit 0; }

started_at="$(date +%s)"
work_dir="$(mktemp -d "$DATA_DIR/.sync.XXXXXX")"
trap 'rm -rf -- "$work_dir"' EXIT

github_api() {
  local -a headers=(-H 'Accept: application/vnd.github+json')
  if [ -n "${BGP_API_GITHUB_TOKEN:-}" ]; then
    headers+=(-H "Authorization: Bearer $BGP_API_GITHUB_TOKEN")
  fi
  curl --fail --location --silent --show-error --retry 3 \
    "${headers[@]}" "https://api.github.com$1"
}

download() {
  local -a headers=()
  if [ -n "${BGP_API_GITHUB_TOKEN:-}" ]; then
    headers+=(-H "Authorization: Bearer $BGP_API_GITHUB_TOKEN")
  fi
  curl --fail --location --silent --show-error --retry 3 \
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
zstd --decompress --stdout "$work_dir/$database_asset" | tar -xf - -C "$work_dir/database"
tar -xzf "$work_dir/$binary_asset" -C "$work_dir/binary"
new_database="$work_dir/database/mehrnet_bgp.bbolt"
new_binary="$work_dir/binary/bgp-api-linux-${arch}"
[ -s "$new_database" ] || die "database archive did not contain mehrnet_bgp.bbolt"
[ -s "$new_binary" ] || die "binary archive did not contain bgp-api-linux-${arch}"
chmod 0755 "$new_binary"

say "validating downloaded database"
BGP_API_DATABASE_PATH="$new_database" BGP_API_VALIDATE_ONLY=1 "$new_binary"

database_backup="${DATABASE_PATH}.rollback"
binary_backup="${BINARY_PATH}.rollback"
rm -f -- "$database_backup" "$binary_backup"
systemctl stop "$SERVICE" >/dev/null 2>&1 || true
[ ! -e "$DATABASE_PATH" ] || mv "$DATABASE_PATH" "$database_backup"
[ ! -e "$BINARY_PATH" ] || mv "$BINARY_PATH" "$binary_backup"

rollback() {
  systemctl stop "$SERVICE" >/dev/null 2>&1 || true
  rm -f -- "$DATABASE_PATH" "$BINARY_PATH"
  [ ! -e "$database_backup" ] || mv "$database_backup" "$DATABASE_PATH"
  [ ! -e "$binary_backup" ] || mv "$binary_backup" "$BINARY_PATH"
  systemctl start "$SERVICE" >/dev/null 2>&1 || true
}

chown bgpapi:bgpapi "$new_database"
chmod 0440 "$new_database"
if ! mv "$new_database" "$DATABASE_PATH" || \
   ! install -o root -g root -m 0755 "$new_binary" "$BINARY_PATH" || \
   ! systemctl start "$SERVICE"; then
  rollback
  die "activation failed; previous release was restored"
fi
sleep 1
if ! systemctl is-active --quiet "$SERVICE"; then
  rollback
  die "the API stopped during activation; previous release was restored"
fi

printf '%s\n' "$release_tag" > "$RELEASE_FILE"
printf '%s\n' "$published_at" > "$PUBLISHED_FILE"
chown bgpapi:bgpapi "$RELEASE_FILE" "$PUBLISHED_FILE"
chmod 0640 "$RELEASE_FILE" "$PUBLISHED_FILE"
rm -f -- "$database_backup" "$binary_backup"
elapsed="$(( $(date +%s) - started_at ))"
say "release $release_tag is active; sync completed in ${elapsed}s"
