#!/usr/bin/env bash
# Update a pinned Compose deployment without recreating its PostgreSQL container.
set -euo pipefail

readonly REPOSITORY="${BGP_API_GITHUB_REPOSITORY:-mehrnet/bgp-api}"
readonly DEPLOY_DIR="${BGP_API_DOCKER_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
readonly ENV_FILE="${BGP_API_DOCKER_ENV_FILE:-$DEPLOY_DIR/.env}"
readonly COMPOSE_FILE="$DEPLOY_DIR/docker-compose.yml"
readonly WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/bgp-api-docker.XXXXXX")"

cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

github_api() {
  local -a headers=(-H 'Accept: application/vnd.github+json')
  if [ -n "${BGP_API_GITHUB_TOKEN:-}" ]; then
    headers+=(-H "Authorization: Bearer $BGP_API_GITHUB_TOKEN")
  fi
  curl --fail --location --silent --show-error --retry 3 "${headers[@]}" "https://api.github.com$1"
}

asset_url() {
  jq -r --arg name "$2" '.assets[] | select(.name == $name) | .browser_download_url' <<<"$1"
}

download_asset() {
  local release_json="$1"
  local name="$2"
  local url
  url="$(asset_url "$release_json" "$name")"
  [ -n "$url" ] && [ "$url" != null ] || die "latest release is missing $name"
  curl --fail --location --silent --show-error --retry 3 --output "$WORK_DIR/$name" "$url"
}

verify_asset() {
  local name="$1"
  local expected
  expected="$(awk -v name="$name" '$2 == name || $2 == "./" name { print; exit }' "$WORK_DIR/SHA256SUMS.txt")"
  [ -n "$expected" ] || die "SHA256SUMS.txt has no entry for $name"
  (cd "$WORK_DIR" && printf '%s\n' "$expected" | sha256sum -c - >&2)
}

valid_image() {
  [[ "$1" =~ ^ghcr\.io/mehrnet/bgp-api(-postgres|-sync)?@sha256:[0-9a-f]{64}$ ]]
}

[ -f "$COMPOSE_FILE" ] || die "missing $COMPOSE_FILE"
[ -f "$ENV_FILE" ] || die "missing $ENV_FILE; start from .env.docker.example"
for command in awk curl docker jq mktemp sha256sum; do require_command "$command"; done

release_json="$(github_api "/repos/$REPOSITORY/releases/latest")"
release_tag="$(jq -r '.tag_name // empty' <<<"$release_json")"
[ -n "$release_tag" ] || die "could not determine the latest release"
download_asset "$release_json" SHA256SUMS.txt
download_asset "$release_json" docker-deployment-manifest.json
verify_asset docker-deployment-manifest.json

manifest="$WORK_DIR/docker-deployment-manifest.json"
[ "$(jq -r '.release // empty' "$manifest")" = "$release_tag" ] || die "deployment manifest release does not match $release_tag"
api_image="$(jq -r '.images.api // empty' "$manifest")"
postgres_image="$(jq -r '.images.postgres // empty' "$manifest")"
sync_image="$(jq -r '.images.sync // empty' "$manifest")"
valid_image "$api_image" || die "deployment manifest has an invalid API image"
valid_image "$postgres_image" || die "deployment manifest has an invalid PostgreSQL image"
valid_image "$sync_image" || die "deployment manifest has an invalid sync image"

runtime_env="$WORK_DIR/.env"
awk '!/^(BGP_API_IMAGE|BGP_API_POSTGRES_IMAGE|BGP_API_SYNC_IMAGE)=/' "$ENV_FILE" > "$runtime_env"
printf 'BGP_API_IMAGE=%s\nBGP_API_POSTGRES_IMAGE=%s\nBGP_API_SYNC_IMAGE=%s\n' \
  "$api_image" "$postgres_image" "$sync_image" >> "$runtime_env"

docker compose --env-file "$runtime_env" --file "$COMPOSE_FILE" pull sync api
docker compose --env-file "$runtime_env" --file "$COMPOSE_FILE" run --rm --no-deps sync
docker compose --env-file "$runtime_env" --file "$COMPOSE_FILE" up -d --no-deps api
install -m 0600 "$runtime_env" "$ENV_FILE"
printf 'updated Docker deployment to %s\n' "$release_tag"
