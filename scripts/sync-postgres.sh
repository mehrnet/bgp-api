#!/usr/bin/env bash
# Import the latest PostgreSQL release into a new schema, then atomically move
# the public lookup_prefixes view to it. Run this from the database server.
set -euo pipefail

readonly REPOSITORY="${BGP_API_GITHUB_REPOSITORY:-mehrnet/bgp-api}"
readonly DATABASE_ROLE="${BGP_API_DATABASE_ROLE:-bgp_api}"
readonly WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/bgp-api-postgres.XXXXXX")"
readonly LOOKUP_VIEW_SELECT="source, prefix_key, prefix_length, start_ip_sort, end_ip_sort, ip_version::integer AS ip_version, registry, country, netname, cidr, asn, region, city, status, allocation_date, created, last_modified, record_source, mnt_by, org, description"
readonly ALLOCATION_VIEW_SELECT="id, start_ip_sort, end_ip_sort, ip_version::integer AS ip_version, registry, country, netname, status, allocation_date, created, last_modified, record_source, mnt_by, org, description"
readonly ROUTE_VIEW_SELECT="id, prefix, prefix_length, start_ip_sort, end_ip_sort, ip_version::integer AS ip_version, origin_asn, asn_number, registry, record_source, mnt_by, org, description"
readonly AUTNUM_COLUMNS="id, asn, asn_number, registry, country, as_name, org, status, created, last_modified, record_source, mnt_by, description"

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
      activated_at TIMESTAMPTZ NOT NULL DEFAULT now()
    );
  " >/dev/null
}

download_release() {
  local tag="$1"
  local release_dir="$WORK_DIR/release"
  mkdir -p "$release_dir"
  local asset_url
  download_asset() {
    local asset_name="$1"
    asset_url="$(jq -r --arg name "$asset_name" '.assets[] | select(.name == $name) | .browser_download_url' <<<"$release_json")"
    [ -n "$asset_url" ] && [ "$asset_url" != "null" ] || die "release $tag is missing $asset_name"
    curl --fail --location --silent --show-error --retry 3 --output "$release_dir/$asset_name" "$asset_url"
  }

  download_asset 'SHA256SUMS.txt'

  [ -s "$release_dir/SHA256SUMS.txt" ] || die "release $tag has no checksum manifest"
  local expected
  expected="$(awk '$2 ~ /(^|\/)mehrnet_bgp_postgres\.sql\.gz/ { sub(/^.*\//, "", $2); print }' "$release_dir/SHA256SUMS.txt")"
  [ -n "$expected" ] || die "release $tag has no PostgreSQL dump checksum"

  local dump="$release_dir/mehrnet_bgp_postgres.sql.gz"
  asset_url="$(jq -r '.assets[] | select(.name == "mehrnet_bgp_postgres.sql.gz") | .browser_download_url' <<<"$release_json")"
  if [ -n "$asset_url" ] && [ "$asset_url" != "null" ]; then
    download_asset 'mehrnet_bgp_postgres.sql.gz'
  else
    local -a part_names
    mapfile -t part_names < <(jq -r '.assets[] | select(.name | startswith("mehrnet_bgp_postgres.sql.gz.part-")) | .name' <<<"$release_json" | sort)
    [ "${#part_names[@]}" -gt 0 ] || die "release $tag has no PostgreSQL dump"
    for part_name in "${part_names[@]}"; do
      download_asset "$part_name"
    done
    local parts=("$release_dir"/mehrnet_bgp_postgres.sql.gz.part-*)
    cat "${parts[@]}" > "$dump"
  fi
  (cd "$release_dir" && printf '%s\n' "$expected" | sha256sum -c - >&2)
  printf '%s\n' "$dump"
}

[ -n "${DATABASE_URL:-}" ] || die "DATABASE_URL is required"
for command in curl gzip jq psql sha256sum; do require_command "$command"; done

initialize_metadata
release_json="$(github_api "/repos/$REPOSITORY/releases/latest")"
latest_tag="$(jq -r '.tag_name' <<<"$release_json")"
[ -n "$latest_tag" ] || die "could not determine the latest release"
new_schema="$(schema_for_tag "$latest_tag")"
active_tag="$(psql_query "SELECT release_tag FROM public.bgp_api_dataset WHERE singleton;")"
active_schema="$(psql_query "SELECT dataset_schema FROM public.bgp_api_dataset WHERE singleton;")"

if [ "$active_tag" = "$latest_tag" ]; then
  printf 'release %s is already active\n' "$latest_tag"
  exit 0
fi

dump="$(download_release "$latest_tag")"
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
for index in idx_lookup_prefix idx_allocation_objects_range idx_route_objects_prefix idx_route_objects_asn_id idx_autnums_asn_number; do
  index_exists="$(psql_query "SELECT to_regclass('$new_schema.$index') IS NOT NULL;")"
  [ "$index_exists" = "t" ] || die "new $index index is missing"
done

psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet <<SQL
BEGIN;
CREATE OR REPLACE VIEW public.lookup_prefixes AS
  SELECT $LOOKUP_VIEW_SELECT FROM "$new_schema".lookup_prefixes;
CREATE OR REPLACE VIEW public.allocation_objects AS
  SELECT $ALLOCATION_VIEW_SELECT FROM "$new_schema".allocation_objects;
CREATE OR REPLACE VIEW public.route_objects AS
  SELECT $ROUTE_VIEW_SELECT FROM "$new_schema".route_objects;
CREATE OR REPLACE VIEW public.autnums AS
  SELECT $AUTNUM_COLUMNS FROM "$new_schema".autnums;
GRANT USAGE ON SCHEMA public TO "$DATABASE_ROLE";
GRANT SELECT ON TABLE public.lookup_prefixes, public.allocation_objects, public.route_objects, public.autnums TO "$DATABASE_ROLE";
INSERT INTO public.bgp_api_dataset (singleton, release_tag, dataset_schema, activated_at)
  VALUES (TRUE, '$latest_tag', '$new_schema', now())
  ON CONFLICT (singleton) DO UPDATE
    SET release_tag = EXCLUDED.release_tag,
        dataset_schema = EXCLUDED.dataset_schema,
        activated_at = EXCLUDED.activated_at;
COMMIT;
SQL

if [[ "$active_schema" =~ ^bgp_[0-9]{8}_[0-9]{4}$ ]] && [ "$active_schema" != "$new_schema" ]; then
  psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet --command "DROP SCHEMA \"$active_schema\" CASCADE;" >/dev/null
fi

printf 'release %s is active in schema %s (%s rows)\n' "$latest_tag" "$new_schema" "$row_count"
