#!/usr/bin/env bash
# Apply verified PostgreSQL release patches to the active schema. A full import
# and view swap remains available only as an explicit bootstrap/recovery mode.
set -euo pipefail

readonly REPOSITORY="${BGP_API_GITHUB_REPOSITORY:-mehrnet/bgp-api}"
readonly DATABASE_ROLE="${BGP_API_DATABASE_ROLE:-bgp_api}"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/bgp-api-postgres.XXXXXX")"
readonly WORK_DIR
readonly RELEASE_DIR="$WORK_DIR/release"
readonly SYNC_MODE="${BGP_API_SYNC_MODE:-patch}"
readonly SYNC_BINARY="${BGP_API_SYNC_BINARY:-1}"
readonly BINARY_PATH="${BGP_API_BINARY_PATH:-/usr/local/bin/bgp-api}"
readonly SERVICE_NAME="${BGP_API_SERVICE_NAME:-bgp-api}"
readonly LOOKUP_VIEW_SELECT="source, prefix_key, prefix_length, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, cidr, asn, region, city, status, allocation_date, created, last_modified, record_source, mnt_by, org, abuse_contact, description"
readonly ALLOCATION_VIEW_SELECT="id, start_ip_sort, end_ip_sort, ip_version, registry, country, netname, status, allocation_date, created, last_modified, record_source, mnt_by, org, abuse_contact, description"
readonly ROUTE_VIEW_SELECT="id, prefix, prefix_length, start_ip_sort, end_ip_sort, ip_version, origin_asn, asn_number, registry, record_source, mnt_by, org, abuse_contact, description"
readonly AUTNUM_COLUMNS="id, asn, asn_number, registry, country, as_name, org, status, created, last_modified, record_source, mnt_by, abuse_contact, description"
readonly RANGE_SUMMARY_COLUMNS="cidr, ip_version, prefix_length, allocation_records, route_records, countries, asns"

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
  local release_dir="$RELEASE_DIR/$tag"
  mkdir -p "$release_dir"
  asset_url="$(asset_url_for "$asset_name")"
  [ -n "$asset_url" ] && [ "$asset_url" != "null" ] || die "release $tag is missing $asset_name"
  curl --fail --location --silent --show-error --retry 3 --output "$release_dir/$asset_name" "$asset_url"
}

download_checksum_manifest() {
  local tag="$1"
  local release_dir="$RELEASE_DIR/$tag"
  if [ ! -s "$release_dir/SHA256SUMS.txt" ]; then
    download_asset "$tag" 'SHA256SUMS.txt'
  fi
  [ -s "$release_dir/SHA256SUMS.txt" ] || die "release $tag has no checksum manifest"
}

checksum_line_for() {
  local tag="$1"
  local asset_name="$2"
  awk -v name="$asset_name" '$2 == name || $2 == "./" name || $2 ~ "/" name "$" { sub(/^.*\//, "", $2); print; exit }' "$RELEASE_DIR/$tag/SHA256SUMS.txt"
}

verify_asset() {
  local tag="$1"
  local asset_name="$2"
  local expected
  download_checksum_manifest "$tag"
  expected="$(checksum_line_for "$tag" "$asset_name")"
  [ -n "$expected" ] || die "release $tag has no checksum for $asset_name"
  (cd "$RELEASE_DIR/$tag" && printf '%s\n' "$expected" | sha256sum -c - >&2)
}

download_asset_or_parts() {
  local tag="$1"
  local asset_name="$2"
  local release_dir="$RELEASE_DIR/$tag"
  if asset_exists "$asset_name"; then
    download_asset "$tag" "$asset_name"
    verify_asset "$tag" "$asset_name"
    printf '%s\n' "$release_dir/$asset_name"
    return
  fi

  local -a part_names
  mapfile -t part_names < <(jq -r --arg name "$asset_name" '.assets[] | select(.name | startswith($name + ".part-")) | .name' <<<"$release_json" | sort)
  [ "${#part_names[@]}" -gt 0 ] || die "release $tag has no $asset_name asset"
  for part_name in "${part_names[@]}"; do
    download_asset "$tag" "$part_name"
    verify_asset "$tag" "$part_name"
  done
  local parts=("$release_dir/$asset_name".part-*)
  cat "${parts[@]}" > "$release_dir/$asset_name"
  printf '%s\n' "$release_dir/$asset_name"
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
  download_asset_or_parts "$tag" 'mehrnet_bgp_postgres.sql.gz'
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
  tar -xzf "$RELEASE_DIR/$tag/$asset" -C "$WORK_DIR/bin" "$binary"
  install -m 0755 "$WORK_DIR/bin/$binary" "$BINARY_PATH.new"
  mv "$BINARY_PATH.new" "$BINARY_PATH"
  if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files "$SERVICE_NAME.service" >/dev/null 2>&1; then
    systemctl restart "$SERVICE_NAME"
  fi
  printf 'installed %s from release %s to %s\n' "$binary" "$tag" "$BINARY_PATH"
}

load_release() {
  local tag="$1"
  release_json="$(github_api "/repos/$REPOSITORY/releases/tags/$tag")"
  [ "$(jq -r '.draft // false' <<<"$release_json")" = "false" ] || die "release $tag is a draft"
  [ "$(jq -r '.prerelease // false' <<<"$release_json")" = "false" ] || die "release $tag is a prerelease"
}

verify_release_time() {
  local tag="$1"
  local published_at published_epoch server_epoch
  published_at="$(jq -r '.published_at // empty' <<<"$release_json")"
  [ -n "$published_at" ] || die "release $tag has no published_at timestamp"
  published_epoch="$(date -u --date="$published_at" +%s 2>/dev/null)" || die "release $tag has an invalid published_at timestamp"
  server_epoch="$(date -u +%s)"
  [ "$published_epoch" -le "$server_epoch" ] || die "release $tag is timestamped later than this server"
}

validate_dataset_schema() {
  local schema="$1"
  local row_count object_count index_exists
  row_count="$(psql_query "SELECT count(*) FROM \"$schema\".lookup_prefixes;")"
  [ "$row_count" -gt 0 ] || die "schema $schema has an empty lookup_prefixes table"
  for table in allocation_objects route_objects autnums range_summaries; do
    object_count="$(psql_query "SELECT count(*) FROM \"$schema\".$table;")"
    [ "$object_count" -gt 0 ] || die "schema $schema has an empty $table table"
  done
  for index in idx_lookup_prefix idx_allocation_objects_overlap idx_route_objects_prefix idx_route_objects_overlap idx_route_objects_asn_id idx_autnums_asn_number; do
    index_exists="$(psql_query "SELECT to_regclass('$schema.$index') IS NOT NULL;")"
    [ "$index_exists" = "t" ] || die "schema $schema is missing $index"
  done
}

record_patch_metadata() {
  local schema="$1"
  local tag="$2"
  local built_at="$3"
  local source_commit="$4"
  psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet \
    --set "dataset_schema=$schema" --set "target_release=$tag" \
    --set "target_built_at=$built_at" --set "target_commit=$source_commit" <<'SQL'
BEGIN;
UPDATE :"dataset_schema".dataset_metadata
  SET release_tag = :'target_release',
      built_at = NULLIF(:'target_built_at', ''),
      source_commit = NULLIF(:'target_commit', '');
INSERT INTO public.bgp_api_dataset (singleton, release_tag, dataset_schema, built_at, source_commit, activated_at)
  VALUES (TRUE, :'target_release', :'dataset_schema', NULLIF(:'target_built_at', ''), NULLIF(:'target_commit', ''), now())
  ON CONFLICT (singleton) DO UPDATE
    SET release_tag = EXCLUDED.release_tag,
        dataset_schema = EXCLUDED.dataset_schema,
        built_at = EXCLUDED.built_at,
        source_commit = EXCLUDED.source_commit,
        activated_at = EXCLUDED.activated_at;
COMMIT;
SQL
}

apply_patch_release() {
  local active_schema="$1"
  local active_tag="$2"
  local target_tag="$3"
  local manifest patch_asset patch format base target source_commit built_at
  load_release "$target_tag"
  verify_release_time "$target_tag"
  download_asset "$target_tag" 'postgres-patch-manifest.json'
  verify_asset "$target_tag" 'postgres-patch-manifest.json'
  manifest="$RELEASE_DIR/$target_tag/postgres-patch-manifest.json"
  format="$(jq -r '.format // empty' "$manifest")"
  base="$(jq -r '.base_release // empty' "$manifest")"
  target="$(jq -r '.target_release // empty' "$manifest")"
  patch_asset="$(jq -r '.asset // empty' "$manifest")"
  [ "$format" = 'mehrnet-bgp-postgres-logical-patch/v1' ] || die "release $target_tag has an unsupported patch format"
  [ "$base" = "$active_tag" ] || die "patch $target_tag expects $base but active dataset is $active_tag"
  [ "$target" = "$target_tag" ] || die "patch target $target does not match release $target_tag"
  [[ "$patch_asset" =~ ^mehrnet_bgp_postgres\.patch\.db-[0-9.]+-[0-9]+-[0-9]+\.sql\.gz$ ]] || die "release $target_tag has an unsafe patch asset name"
  patch="$(download_asset_or_parts "$target_tag" "$patch_asset")"
  gzip -t "$patch"

  printf 'applying patch %s -> %s\n' "$active_tag" "$target_tag"
  # The first v1 patch was produced before its lookup delete had an explicit
  # equality join. source and prefix_key are both non-null, so this preserves
  # its meaning while allowing PostgreSQL to use idx_lookup_prefix.
  gzip -dc "$patch" | sed 's/target\.source IS NOT DISTINCT FROM patch\.source AND target\.prefix_key IS NOT DISTINCT FROM patch\.prefix_key/target.source = patch.source AND target.prefix_key = patch.prefix_key/' |
    psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet --set "dataset_schema=$active_schema"
  validate_dataset_schema "$active_schema"
  source_commit="$(jq -r '.source_commit // empty' "$manifest")"
  built_at="$(jq -r '.built_at // empty' "$manifest")"
  if [ -z "$built_at" ]; then
    built_at="$(jq -r '.published_at // empty' <<<"$release_json")"
  fi
  record_patch_metadata "$active_schema" "$target_tag" "$built_at" "$source_commit"
}

patch_chain_from() {
  local active_tag="$1"
  local releases_json seen=false tag
  releases_json="$(github_api "/repos/$REPOSITORY/releases?per_page=100")"
  mapfile -t release_tags < <(jq -r '[.[] | select((.draft // false) == false and (.prerelease // false) == false) | {tag: .tag_name, published: .published_at}] | sort_by(.published) | .[].tag' <<<"$releases_json")
  [ "${#release_tags[@]}" -gt 0 ] || die "GitHub has no published BGP dataset releases"
  for tag in "${release_tags[@]}"; do
    if [ "$tag" = "$active_tag" ]; then
      seen=true
      continue
    fi
    if [ "$seen" = true ]; then
      printf '%s\n' "$tag"
    fi
  done
  [ "$seen" = true ] || die "active release $active_tag is not in the recent release history; rerun with BGP_API_SYNC_MODE=snapshot"
}

sync_snapshot() {
  local latest_tag="$1"
  local active_schema="$2"
  local new_schema dump schema_exists built_at source_commit row_count
  new_schema="$(schema_for_tag "$latest_tag")"
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
  validate_dataset_schema "$new_schema"
  row_count="$(psql_query "SELECT count(*) FROM \"$new_schema\".lookup_prefixes;")"
  [ "$(psql_query "SELECT count(*) FROM \"$new_schema\".dataset_metadata;")" -eq 1 ] || die "new dataset metadata is invalid"
  built_at="$(psql_query "SELECT coalesce(built_at, '') FROM \"$new_schema\".dataset_metadata LIMIT 1;")"
  source_commit="$(psql_query "SELECT coalesce(source_commit, '') FROM \"$new_schema\".dataset_metadata LIMIT 1;")"

  psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet <<SQL
BEGIN;
DROP VIEW IF EXISTS public.lookup_prefixes, public.allocation_objects, public.route_objects, public.autnums, public.range_summaries;
CREATE VIEW public.lookup_prefixes AS
  SELECT $LOOKUP_VIEW_SELECT FROM "$new_schema".lookup_prefixes;
CREATE VIEW public.allocation_objects AS
  SELECT $ALLOCATION_VIEW_SELECT FROM "$new_schema".allocation_objects;
CREATE VIEW public.route_objects AS
  SELECT $ROUTE_VIEW_SELECT FROM "$new_schema".route_objects;
CREATE VIEW public.autnums AS
  SELECT $AUTNUM_COLUMNS FROM "$new_schema".autnums;
CREATE VIEW public.range_summaries AS
  SELECT $RANGE_SUMMARY_COLUMNS FROM "$new_schema".range_summaries;
GRANT USAGE ON SCHEMA public TO "$DATABASE_ROLE";
GRANT SELECT ON TABLE public.lookup_prefixes, public.allocation_objects, public.route_objects, public.autnums, public.range_summaries TO "$DATABASE_ROLE";
INSERT INTO public.bgp_api_dataset (singleton, release_tag, dataset_schema, built_at, source_commit, activated_at)
  VALUES (TRUE, '$latest_tag', '$new_schema', NULLIF('$built_at', ''), NULLIF('$source_commit', ''), now())
  ON CONFLICT (singleton) DO UPDATE
    SET release_tag = EXCLUDED.release_tag,
        dataset_schema = EXCLUDED.dataset_schema,
        built_at = EXCLUDED.built_at,
        source_commit = EXCLUDED.source_commit,
        activated_at = EXCLUDED.activated_at;
COMMIT;
SQL

  if [[ "$active_schema" =~ ^bgp_[0-9]{8}_[0-9]{4}$ ]] && [ "$active_schema" != "$new_schema" ]; then
    psql "$DATABASE_URL" --set ON_ERROR_STOP=1 --quiet --command "DROP SCHEMA IF EXISTS \"$active_schema\" CASCADE;" >/dev/null
  fi
  printf 'release %s is active in schema %s (%s rows)\n' "$latest_tag" "$new_schema" "$row_count"
}

[ -n "${DATABASE_URL:-}" ] || die "DATABASE_URL is required"
for command in curl date gzip install jq psql sed sha256sum tar uname; do require_command "$command"; done
case "$SYNC_MODE" in
  patch|snapshot) ;;
  *) die "BGP_API_SYNC_MODE must be patch or snapshot" ;;
esac

initialize_metadata
release_json="$(github_api "/repos/$REPOSITORY/releases/latest")"
latest_tag="$(jq -r '.tag_name // empty' <<<"$release_json")"
[ -n "$latest_tag" ] || die "could not determine the latest release"
verify_release_time "$latest_tag"
active_tag="$(psql_query "SELECT release_tag FROM public.bgp_api_dataset WHERE singleton;")"
active_schema="$(psql_query "SELECT dataset_schema FROM public.bgp_api_dataset WHERE singleton;")"

if [ "$active_tag" = "$latest_tag" ]; then
  printf 'release %s is already active\n' "$latest_tag"
  exit 0
fi

if [ "$SYNC_MODE" = snapshot ]; then
  sync_snapshot "$latest_tag" "$active_schema"
  load_release "$latest_tag"
  sync_binary "$latest_tag"
else
  [ -n "$active_tag" ] && [ -n "$active_schema" ] || die "no active dataset; bootstrap with BGP_API_SYNC_MODE=snapshot"
  validate_dataset_schema "$active_schema"
  mapfile -t patch_chain < <(patch_chain_from "$active_tag")
  [ "${#patch_chain[@]}" -gt 0 ] || die "no release newer than active dataset $active_tag was found"
  for target_tag in "${patch_chain[@]}"; do
    apply_patch_release "$active_schema" "$active_tag" "$target_tag"
    active_tag="$target_tag"
  done
  load_release "$latest_tag"
  sync_binary "$latest_tag"
  printf 'release %s is active in schema %s after %s patch(es)\n' "$active_tag" "$active_schema" "${#patch_chain[@]}"
fi
