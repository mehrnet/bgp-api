#!/usr/bin/env bash
set -euo pipefail

# Runs the same persistent-connection benchmark against the private loopback
# origin and the public Cloudflare HTTPS endpoint from a second network.
ORIGIN_HOST="${BGP_API_ORIGIN_HOST:-root@78.31.250.179}"
ORIGIN_IP="${BGP_API_ORIGIN_IP:-78.31.250.179}"
EXTERNAL_HOST="${BGP_API_EXTERNAL_HOST:-root@138.199.236.120}"
PUBLIC_URL="${BGP_API_PUBLIC_URL:-https://bgp-api.mehrnet.com/v1/ip?query=80.93.223.205}"
REQUESTS="${BGP_API_BENCH_REQUESTS:-5000}"
CONCURRENCY="${BGP_API_BENCH_CONCURRENCY:-32}"
WARMUP="${BGP_API_BENCH_WARMUP:-200}"
BENCHMARK_BINARY="${BGP_API_BENCHMARK_BINARY:-$PWD/bin/bgp-api-benchmark}"

go build -trimpath -ldflags='-s -w' -o "$BENCHMARK_BINARY" ./cmd/benchmark-http

origin_slot="$(ssh "$ORIGIN_HOST" 'cat /var/lib/bgp-api/active-slot')"
case "$origin_slot" in
  primary) origin_port=3102; origin_env=/etc/bgp-api/bgp-api.env ;;
  secondary) origin_port=3103; origin_env=/etc/bgp-api/bgp-api-secondary.env ;;
  *) printf 'invalid active slot: %s\n' "$origin_slot" >&2; exit 1 ;;
esac
origin_token="$(ssh "$ORIGIN_HOST" "sed -n 's/^ORIGIN_AUTH_TOKEN=//p' '$origin_env' | head -n1")"
if [ -z "$origin_token" ]; then
  printf 'could not read the origin authorization token\n' >&2
  exit 1
fi

ssh "$ORIGIN_HOST" 'mkdir -p /tmp/bgp-api-benchmark'
ssh "$EXTERNAL_HOST" 'mkdir -p /tmp/bgp-api-benchmark'
scp -q "$BENCHMARK_BINARY" "$ORIGIN_HOST:/tmp/bgp-api-benchmark/benchmark"
scp -q "$BENCHMARK_BINARY" "$EXTERNAL_HOST:/tmp/bgp-api-benchmark/benchmark"

printf '\n== loopback origin (private HTTP) ==\n'
ssh "$ORIGIN_HOST" "/tmp/bgp-api-benchmark/benchmark -url 'http://127.0.0.1:${origin_port}/v1/ip?query=80.93.223.205' -requests '$REQUESTS' -concurrency '$CONCURRENCY' -warmup '$WARMUP' -header 'X-BGP-API-Origin-Token: $origin_token'"

printf '\n== external client (direct-origin HTTPS) ==\n'
ssh "$EXTERNAL_HOST" "/tmp/bgp-api-benchmark/benchmark -url '$PUBLIC_URL' -connect-ip '$ORIGIN_IP' -requests '$REQUESTS' -concurrency '$CONCURRENCY' -warmup '$WARMUP'"

printf '\n== external client (public HTTPS through Cloudflare) ==\n'
ssh "$EXTERNAL_HOST" "/tmp/bgp-api-benchmark/benchmark -url '$PUBLIC_URL' -requests '$REQUESTS' -concurrency '$CONCURRENCY' -warmup '$WARMUP'"
