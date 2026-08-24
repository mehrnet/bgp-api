#!/usr/bin/env bash
# Import the latest PostgreSQL release into a new schema, then atomically move
# the public lookup_prefixes view to it. Run this from the database server.
set -euo pipefail

readonly REPOSITORY="${BGP_API_GITHUB_REPOSITORY:-mehrnet/bgp-api}"
readonly DATABASE_ROLE="${BGP_API_DATABASE_ROLE:-bgp_api}"
readonly WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/bgp-api-postgres.XXXXXX")"
readonly RELEASE_DIR="$WORK_DIR/release"
readonly SYNC_BINARY="${BGP_API_SYNC_BINARY:-1}"
readonly BINARY_PATH="${BGP_API_BINARY_PATH:-/usr/local/bin/bgp-api}"
readonly SERVICE_NAME="${BGP_API_SERVICE_NAME:-bgp-api}"
readonly LOOKUP_VIEW_SELECT="source, prefix_key, prefix_length, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, cidr, asn, region, city, status, allocation_date, created, last_modified, record_source, mnt_by, org, abuse_contact, description"
readonly ALLOCATION_VIEW_SELECT="id, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, status, allocation_date, created, last_modified, record_source, mnt_by, org, abuse_contact, description"
readonly ROUTE_VIEW_SELECT="id, prefix, prefix_length, start_ip_sort, end_ip_sort, ip_version, origin_asn, asn_number, registry, record_source, mnt_by, org, abuse_contact, description"
readonly AUTNUM_COLUMNS="id, asn, asn_number, registry, country, as_name, org, status, created, last_modified, record_source, mnt_by, abuse_contact, description"

cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

[[ "$DATABASE_ROLE" =~ ^[a-z_][a-z0-9_]*$ ]] || die "BGP_API_DATABASE_ROLE must be a PostgreSQL role identifier"

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

github_api() {
  local endpoint="$1"
  local -a headers=(-H 'Accept: application/vnd.github+json')
  if [ -n "${BGP_API_GITHUB_TOKEN:-}" ]; then
    headers+=(-H "Authorization: Bearer $BGP_API_GITHUB_TOKEN")
  fi
  curl --fail --location --silent --show-error --retry 3 "${headers[@]}" "https://api.github.com$endpoint"
}

asset_url_for() {
  local asset_name="$1"
  jq -r --arg name "$asset_name" '.assets[] | select(.name == $name) | .browser_download_url' <<<"$release_json"
}

asset_exists() {
  local asset_name="$1"
  local asset_url
  asset_url="$(asset_url_for "$asset_name")"
  [ -n "$asset_url" ] && [ "$asset_url" != "null" ]
}

download_asset() {
  local tag="$1"
  local asset_name="$2"
  local asset_url
  mkdir -p "$RELEASE_DIR"
  asset_url="$(asset_url_for "$asset_name")"
  [ -n "$asset_url" ] && [ "$asset_url" != "null" ] || die "release $tag is missing $asset_name"
  curl --fail --location --silent --show-error --retry 3 --output "$RELEASE_DIR/$asset_name" "$asset_url"
}

download_checksum_manifest() {
  local tag="$1"
  if [ ! -s "$RELEASE_DIR/SHA256SUMS.txt" ]; then
    download_asset "$tag" 'SHA256SUMS.txt'
  fi
  [ -s "$RELEASE_DIR/SHA256SUMS.txt" ] || die "release $tag has no checksum manifest"
}

checksum_line_for() {
  local asset_name="$1"
  awk -v name="$asset_name" '$2 == name || $2 == "./" name || $2 ~ "/" name "$" { sub(/^.*\//, "", $2); print; exit }' "$RELEASE_DIR/SHA256SUMS.txt"
}

verify_asset() {
  local tag="$1"
  local asset_name="$2"
  local expected
  download_checksum_manifest "$tag"
  expected="$(checksum_line_for "$asset_name")"
  [ -n "$expected" ] || die "release $tag has no checksum for $asset_name"
  (cd "$RELEASE_DIR" && printf '%s\n' "$expected" | sha256sum -c - >&2)
}

schema_for_tag() {
  local tag="$1"
  if [[ "$tag" =~ ^db-([0-9]{4})\.([0-9]{2})\.([0-9]{2})-([0-9]{4})-[0-9]+$ ]]; then
    printf 'bgp_%s%s%s_%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}" "${BASH_REMATCH[4]}"
    return
  fi
  die "unrecognized release tag: $tag"
}

psql_query() {
  psql "$DATABASE_URL" --no-align --tuples-only --quiet --command "$1"
}

initialize_metadata() {
  psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet --command "
    CREATE TABLE IF NOT EXISTS public.bgp_api_dataset (
      singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
      release_tag TEXT NOT NULL,
      dataset_schema TEXT NOT NULL,
      built_at TEXT,
      source_commit TEXT,
      activated_at TIMESTAMPTZ NOT NULL DEFAULT now()
    );
    ALTER TABLE public.bgp_api_dataset ADD COLUMN IF NOT EXISTS built_at TEXT;
    ALTER TABLE public.bgp_api_dataset ADD COLUMN IF NOT EXISTS source_commit TEXT;
  " >/dev/null
}

download_postgres_dump() {
  local tag="$1"
  local dump="$RELEASE_DIR/mehrnet_bgp_postgres.sql.gz"
  download_checksum_manifest "$tag"
  if asset_exists 'mehrnet_bgp_postgres.sql.gz'; then
    download_asset "$tag" 'mehrnet_bgp_postgres.sql.gz'
    verify_asset "$tag" 'mehrnet_bgp_postgres.sql.gz'
  else
    local -a part_names
    mapfile -t part_names < <(jq -r '.assets[] | select(.name | startswith("mehrnet_bgp_postgres.sql.gz.part-")) | .name' <<<"$release_json" | sort)
    [ "${#part_names[@]}" -gt 0 ] || die "release $tag has no PostgreSQL dump"
    for part_name in "${part_names[@]}"; do
      download_asset "$tag" "$part_name"
      verify_asset "$tag" "$part_name"
    done
    local parts=("$RELEASE_DIR"/mehrnet_bgp_postgres.sql.gz.part-*)
    cat "${parts[@]}" > "$dump"
  fi
  printf '%s\n' "$dump"
}

binary_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    *) return 1 ;;
  esac
}

truthy() {
  case "${1,,}" in
    1|true|yes|on) return 0 ;;
    *) return 1 ;;
  esac
}

sync_binary() {
  local tag="$1"
  truthy "$SYNC_BINARY" || return 0

  local arch
  arch="$(binary_arch)" || {
    printf 'skipping API binary sync: unsupported machine architecture %s\n' "$(uname -m)" >&2
    return 0
  }

  local binary="bgp-api-linux-$arch"
  local asset="$binary.tar.gz"
  if ! asset_exists "$asset"; then
    printf 'skipping API binary sync: release %s has no %s asset\n' "$tag" "$asset" >&2
    return 0
  fi

  download_asset "$tag" "$asset"
  verify_asset "$tag" "$asset"
  mkdir -p "$WORK_DIR/bin"
  tar -xzf "$RELEASE_DIR/$asset" -C "$WORK_DIR/bin" "$binary"
  install -m 0755 "$WORK_DIR/bin/$binary" "$BINARY_PATH.new"
  mv "$BINARY_PATH.new" "$BINARY_PATH"
  if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files "$SERVICE_NAME.service" >/dev/null 2>&1; then
    systemctl restart "$SERVICE_NAME"
  fi
  printf 'installed %s from release %s to %s\n' "$binary" "$tag" "$BINARY_PATH"
}

[ -n "${DATABASE_URL:-}" ] || die "DATABASE_URL is required"
for command in curl date gzip install jq psql sha256sum tar uname; do require_command "$command"; done

initialize_metadata
release_json="$(github_api "/repos/$REPOSITORY/releases/latest")"
latest_tag="$(jq -r '.tag_name' <<<"$release_json")"
[ -n "$latest_tag" ] || die "could not determine the latest release"
release_published_at="$(jq -r '.published_at // empty' <<<"$release_json")"
[ -n "$release_published_at" ] || die "release $latest_tag has no published_at timestamp"
release_published_epoch="$(date -u --date="$release_published_at" +%s 2>/dev/null)" || die "release $latest_tag has an invalid published_at timestamp"
server_epoch="$(date -u +%s)"
[ "$release_published_epoch" -le "$server_epoch" ] || die "release $latest_tag is timestamped later than this server"
new_schema="$(schema_for_tag "$latest_tag")"
active_tag="$(psql_query "SELECT release_tag FROM public.bgp_api_dataset WHERE singleton;")"
active_schema="$(psql_query "SELECT dataset_schema FROM public.bgp_api_dataset WHERE singleton;")"

if [ "$active_tag" = "$latest_tag" ]; then
	printf 'release %s is already active\n' "$latest_tag"
	exit 0
fi

dump="$(download_postgres_dump "$latest_tag")"
schema_exists="$(psql_query "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = '$new_schema');")"
if [ "$schema_exists" = "t" ]; then
  psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet --command "DROP SCHEMA \"$new_schema\" CASCADE;" >/dev/null
fi

printf 'importing %s into schema %s\n' "$latest_tag" "$new_schema"
# The producer normalizes text to valid UTF-8 before emitting PostgreSQL COPY
# data. Avoid iconv here: on a multi-gigabyte stream it can buffer enough data
# to exhaust a small production host.
gzip -dc "$dump" | psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet

row_count="$(psql_query "SELECT count(*) FROM \"$new_schema\".lookup_prefixes;")"
[ "$row_count" -gt 0 ] || die "new lookup_prefixes table is empty"
for table in allocation_objects route_objects autnums; do
  object_count="$(psql_query "SELECT count(*) FROM \"$new_schema\".$table;")"
  [ "$object_count" -gt 0 ] || die "new $table table is empty"
done
metadata_count="$(psql_query "SELECT count(*) FROM \"$new_schema\".dataset_metadata;")"
[ "$metadata_count" -eq 1 ] || die "new dataset_metadata table must have exactly one row"
for index in idx_lookup_prefix idx_allocation_objects_overlap idx_route_objects_prefix idx_route_objects_overlap idx_route_objects_asn_id idx_autnums_asn_number; do
  index_exists="$(psql_query "SELECT to_regclass('$new_schema.$index') IS NOT NULL;")"
  [ "$index_exists" = "t" ] || die "new $index index is missing"
done
built_at="$(psql_query "SELECT coalesce(built_at, '') FROM \"$new_schema\".dataset_metadata LIMIT 1;")"
source_commit="$(psql_query "SELECT coalesce(source_commit, '') FROM \"$new_schema\".dataset_metadata LIMIT 1;")"

psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet <<SQL
BEGIN;
DROP VIEW IF EXISTS public.lookup_prefixes, public.allocation_objects, public.route_objects, public.autnums;
CREATE VIEW public.lookup_prefixes AS
  SELECT $LOOKUP_VIEW_SELECT FROM "$new_schema".lookup_prefixes;
CREATE VIEW public.allocation_objects AS
  SELECT $ALLOCATION_VIEW_SELECT FROM "$new_schema".allocation_objects;
CREATE VIEW public.route_objects AS
  SELECT $ROUTE_VIEW_SELECT FROM "$new_schema".route_objects;
CREATE VIEW public.autnums AS
  SELECT $AUTNUM_COLUMNS FROM "$new_schema".autnums;
GRANT USAGE ON SCHEMA public TO "$DATABASE_ROLE";
GRANT SELECT ON TABLE public.lookup_prefixes, public.allocation_objects, public.route_objects, public.autnums TO "$DATABASE_ROLE";
INSERT INTO public.bgp_api_dataset (singleton, release_tag, dataset_schema, activated_at)
  VALUES (TRUE, '$latest_tag', '$new_schema', now())
  ON CONFLICT (singleton) DO UPDATE
    SET release_tag = EXCLUDED.release_tag,
        dataset_schema = EXCLUDED.dataset_schema,
        activated_at = EXCLUDED.activated_at;
UPDATE public.bgp_api_dataset
  SET built_at = NULLIF('$built_at', ''),
      source_commit = NULLIF('$source_commit', '')
  WHERE singleton;
COMMIT;
SQL

if [[ "$active_schema" =~ ^bgp_[0-9]{8}_[0-9]{4}$ ]] && [ "$active_schema" != "$new_schema" ]; then
  psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet --command "DROP SCHEMA \"$active_schema\" CASCADE;" >/dev/null
fi

sync_binary "$latest_tag"
printf 'release %s is active in schema %s (%s rows)\n' "$latest_tag" "$new_schema" "$row_count"
