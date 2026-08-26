#!/bin/sh
set -eu

archive=/usr/local/share/mehrnet-pgdata.tar.zst
marker="$PGDATA/.mehrnet-extracting"

if [ -e "$marker" ] || [ ! -s "$PGDATA/PG_VERSION" ]; then
	printf '%s\n' 'Extracting the pre-indexed MehrNet PostgreSQL snapshot...'
	mkdir -p "$PGDATA"
	find "$PGDATA" -mindepth 1 -maxdepth 1 -exec rm -rf -- '{}' +
	touch "$marker"
	tar --zstd --extract --file "$archive" --directory "$PGDATA"
	rm -f "$marker"
	printf '%s\n' 'MehrNet PostgreSQL snapshot extraction completed.'
fi

exec docker-entrypoint.sh "$@"
