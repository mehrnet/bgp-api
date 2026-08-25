FROM postgres:15.13-bookworm@sha256:d1f651d3a24650cd68a4e7fcf4eef7bd772c2ec965c3563f36b91c1fecddac76

RUN apt-get update && apt-get install -y --no-install-recommends curl jq && rm -rf /var/lib/apt/lists/*
COPY scripts/sync-postgres.sh /usr/local/bin/sync-postgres
RUN chmod 0755 /usr/local/bin/sync-postgres
ENTRYPOINT ["/usr/local/bin/sync-postgres"]
