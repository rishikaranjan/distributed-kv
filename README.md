# Distributed KV (Raft + Sharding + Router)  

A local, distributed key–value store built on Raft with sharding via a consistent-hash router, HTTP gateway, and full observability (Prometheus + Grafana). Includes a simple TrueTime-ish endpoint and a 2PC demo for cross-region commit.

## Features
- Raft replication (3 regions) with commit index & term metrics
- Sharding via router (consistent hashing)
- HTTP gateway proxying to gRPC router
- Prometheus metrics & Grafana dashboard
- TrueTime-style clock bounds
- 2PC coordinator demo

## Quickstart
# 0) Build & start 3 regions
make start
make status
make logs     # Ctrl-C when you see metrics & gRPC ports

# 1) Router
make start-router ROUTER_PORT=6000
make logs-router  # Ctrl-C after “listening … admin :7000”

# 2) HTTP gateway
make restart-gw GW_PORT=8081 GW_GRPC=127.0.0.1:6000

# 3) Prometheus
prometheus --config.file=ops/prometheus.yml --web.listen-address=:9090 &

# 4) Grafana (brew services or app), then import:
# ops/grafana/dashboards/dkv-local-dev-overview.json
