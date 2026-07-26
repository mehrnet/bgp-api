#!/usr/bin/env bash
# Import the latest PostgreSQL release into a new schema, then atomically move
# the public lookup_prefixes view to it. Run this from the database server.
set -euo pipefail

readonly REPOSITORY="${BGP_API_GITHUB_REPOSITORY:-mehrnet/bgp-api}"
readonly WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/bgp-api-postgres.XXXXXX")"
readonly COLUMNS="source, prefix_key, prefix_length, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, cidr, asn, region, city"

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
  gh release download "$tag" --repo "$REPOSITORY" --dir "$release_dir" --clobber \
    --pattern 'SHA256SUMS.txt' \
    --pattern 'mehrnet_bgp_postgres.sql.gz*'

  [ -s "$release_dir/SHA256SUMS.txt" ] || die "release $tag has no checksum manifest"
  local expected
  expected="$(awk '$2 ~ /(^|\/)mehrnet_bgp_postgres\.sql\.gz/ { sub(/^.*\//, "", $2); print }' "$release_dir/SHA256SUMS.txt")"
  [ -n "$expected" ] || die "release $tag has no PostgreSQL dump checksum"
  (cd "$release_dir" && printf '%s\n' "$expected" | sha256sum -c -)

  local dump="$release_dir/mehrnet_bgp_postgres.sql.gz"
  if [ ! -f "$dump" ]; then
    shopt -s nullglob
    local parts=("$release_dir"/mehrnet_bgp_postgres.sql.gz.part-*)
    shopt -u nullglob
    [ "${#parts[@]}" -gt 0 ] || die "release $tag has no PostgreSQL dump"
    cat "${parts[@]}" > "$dump"
  fi
  printf '%s\n' "$dump"
}

[ -n "${DATABASE_URL:-}" ] || die "DATABASE_URL is required"
for command in gh gzip psql sha256sum; do require_command "$command"; done

initialize_metadata
latest_tag="$(gh api "repos/$REPOSITORY/releases/latest" --jq '.tag_name')"
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
gzip -dc "$dump" | psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet

row_count="$(psql_query "SELECT count(*) FROM \"$new_schema\".lookup_prefixes;")"
[ "$row_count" -gt 0 ] || die "new lookup_prefixes table is empty"
index_exists="$(psql_query "SELECT to_regclass('$new_schema.idx_lookup_prefix') IS NOT NULL;")"
[ "$index_exists" = "t" ] || die "new lookup_prefixes index is missing"

psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet <<SQL
BEGIN;
CREATE OR REPLACE VIEW public.lookup_prefixes AS
  SELECT $COLUMNS FROM "$new_schema".lookup_prefixes;
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
