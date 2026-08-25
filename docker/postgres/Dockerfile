FROM postgres:15.13-bookworm@sha256:d1f651d3a24650cd68a4e7fcf4eef7bd772c2ec965c3563f36b91c1fecddac76 AS build

ARG POSTGRES_SCHEMA
ENV PGDATA=/var/lib/postgresql/mehrnet-data

COPY --chown=postgres:postgres mehrnet_bgp_postgres.sql.gz /tmp/dataset.sql.gz
COPY --chown=postgres:postgres bootstrap.sql /tmp/bootstrap.sql
COPY --chown=postgres:postgres build-snapshot.sh /usr/local/bin/build-snapshot

USER postgres
RUN chmod 0755 /usr/local/bin/build-snapshot && POSTGRES_SCHEMA="$POSTGRES_SCHEMA" /usr/local/bin/build-snapshot

FROM postgres:15.13-bookworm@sha256:d1f651d3a24650cd68a4e7fcf4eef7bd772c2ec965c3563f36b91c1fecddac76

ENV PGDATA=/var/lib/postgresql/mehrnet-data
COPY --from=build --chown=postgres:postgres /var/lib/postgresql/mehrnet-data /var/lib/postgresql/mehrnet-data

USER postgres
