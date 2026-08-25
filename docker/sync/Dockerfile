FROM postgres:15.13-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends curl jq && rm -rf /var/lib/apt/lists/*
COPY scripts/sync-postgres.sh /usr/local/bin/sync-postgres
RUN chmod 0755 /usr/local/bin/sync-postgres
ENTRYPOINT ["/usr/local/bin/sync-postgres"]
