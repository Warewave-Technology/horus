# horus

Small, fast internal DNS server.

- Authoritative for internal zones (A, AAAA, CNAME), forwards everything else
  upstream with a TTL-aware cache.
- SQLite storage, REST API + CLI, single static binary, runs under systemd.
- Kubernetes operator: annotate an Ingress with
  `horus.warewave.tech/register: "true"` and its hosts resolve to the
  Ingress load balancer. Multi-cluster, one zone per cluster, first-wins names.

```
cmd/horus/              server + CLI (one binary)
internal/zone/          in-memory snapshot + resolution rules (lock-free reads)
internal/dnsserver/     miekg/dns handler, forwarder, cache wiring
internal/store/         SQLite store, schema, first-wins diff (plan.go)
internal/httpapi/       REST API, auth, write-then-reload
internal/cache/         TTL cache + singleflight
internal/config/        HORUS_* env config
internal/sdnotify/      systemd notify + watchdog (no deps)
internal/metrics/       tiny Prometheus exposition (no deps)
pkg/api/                wire types + validation (shared with the operator)
pkg/client/             Go API client (CLI + operator)
pkg/ingressdns/         Ingress -> desired records (pure, shared)
operator/               Kubernetes operator (separate Go module)
deploy/systemd/         units + env example
docs/                   DESIGN.md, API.md
```

## Quick start

```sh
make tidy          # fetch dependencies (first time)
make vet test operator-test
sudo make install  # binary, unit, /etc/horus/{horus.env,admin.token}
sudo systemctl enable --now horus horus-backup.timer

sudo horus zone add prod.internal.example
sudo horus record add db.prod.internal.example A 10.0.0.5
dig @127.0.0.1 db.prod.internal.example

sudo horus cluster add prod --zone prod.internal.example   # token for the operator
```

Operator deployment: see `operator/config/operator.yaml` and `docs/DESIGN.md`.
