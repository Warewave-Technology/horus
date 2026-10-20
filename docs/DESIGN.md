# horus — design

horus is a small internal DNS server. It is authoritative for a handful of
internal zones, forwards everything else to upstream resolvers, stores its data
in SQLite, is managed through a REST API and a CLI, and runs under systemd.
A Kubernetes operator (one per cluster) registers annotated Ingresses.

Scale target: ~20 human users, a few clusters, hundreds (not millions) of
records. Simplicity beats throughput everywhere below.

## Decisions (agreed, do not change without discussion)

| Topic | Decision |
|---|---|
| Record types | A, AAAA, CNAME only. No wildcards. |
| DNSSEC | No. |
| HA | No. Single node, `Restart=on-failure`, daily SQLite backups. |
| Upstream | Plain forwarder (not a recursive resolver), ordered failover, cache. |
| Kubernetes | Operator watches Ingresses with `horus.warewave.tech/register: "true"`. Ingress controllers sit behind a LoadBalancer, so the target comes from `status.loadBalancer`. |
| Multi-cluster | Yes. Each cluster has its own token, bound to one or more zones. |
| Zones | A cluster may own several zones; a zone belongs to at most one cluster. Zones can be added to or removed from a cluster at runtime. Manual (CLI) records may also live in a cluster's zones. |
| Name conflicts | First-wins. A name belongs to exactly one owner. Conflicting names are skipped and reported; the rest of the Ingress is still applied. |
| Admin auth | One admin token. Only the owner has shell access to the host. |
| Config | `HORUS_*` environment variables (systemd `EnvironmentFile=`), no config file format. |
| Language/libs | Go. `miekg/dns`, `modernc.org/sqlite` (cgo-free). Operator: controller-runtime in a separate Go module. |

## Architecture

```
                 ┌──────────────────────── horus serve (systemd, Type=notify) ───────────────────────┐
 clients ─UDP/TCP:53─► dnsserver.Handler                                                            │
                 │        │  Lookup (lock-free)                                                     │
                 │        ▼                                                                         │
                 │   zone.Holder ── atomic.Pointer ──► zone.Snapshot (immutable, in memory)         │
                 │        │ not authoritative                      ▲                                │
                 │        ▼                                        │ rebuild after each write       │
                 │   dnsserver.Forwarder ── cache + singleflight ──┼──► upstreams                   │
                 │                                                 │                                │
 horus CLI ─unix socket─► httpapi ── write lock ──► store (SQLite, WAL, 1 conn) ──► LoadAll ─┘       │
 operators ─HTTPS:8053──►    │                                                                      │
                 │        audit_log                                                                 │
                 └──────────────────────────────────────────────────────────────────────────────────┘
```

### Read path (DNS)

1. `Handler.ServeDNS` rejects non-QUERY opcodes, multi-question messages,
   non-IN classes, and EDNS versions other than 0.
2. `Snapshot.Lookup(qname, qtype)` decides:
   - not inside any zone → forward;
   - name exists with matching type → answer;
   - name has a CNAME → follow it inside our zones (max 8 hops, loop-safe);
     if the target is outside our zones, the handler resolves the target
     upstream and appends it (`ChaseTarget`);
   - name exists but not that type, or is an empty non-terminal → NODATA;
   - otherwise → NXDOMAIN.
   Negative answers carry a synthesized SOA in the authority section.
3. The SOA's MINIMUM is `min(zone default TTL, 30s)`, so a client that cached
   an NXDOMAIN sees a freshly registered Ingress within 30 seconds.
4. EDNS0: we advertise 1232 bytes; UDP responses are truncated to the client's
   advertised size (min 512, max 1232).

The DNS path never touches SQLite and never takes a lock.

### Write path (API)

1. Every write goes through `httpapi.Server.write`, which holds a mutex, calls
   the store (one SQLite transaction), then rebuilds the snapshot from
   `store.LoadAll` and swaps it in. A 2xx response means the change is live.
2. Owner writes that change nothing (the common operator case) skip the
   rebuild (`writeIf`).
3. Each write bumps the affected zone's serial and appends to `audit_log`.

At this scale a full rebuild per write is microseconds to milliseconds; do
not optimize it until it shows up in a profile.

### Forwarder

- Upstreams are tried in order, starting from the one that answered last.
  A failed exchange, or an rcode other than NOERROR/NXDOMAIN, moves on to the
  next upstream. All failing → SERVFAIL.
- UDP first, TCP retry on truncation. EDNS0 1232.
- Cache key: lowercased qname + qtype + qclass. Packed messages are cached;
  TTLs are decremented on read.
- Positive TTL = min TTL in the message (excluding OPT), capped by
  `HORUS_CACHE_MAX_TTL`. Negative TTL = min(SOA TTL, SOA MINIMUM) per RFC 2308,
  capped by `HORUS_CACHE_NEG_TTL`. No SOA → not cached.
- Identical concurrent queries share one upstream exchange (singleflight).
- `SIGHUP` (`systemctl reload horus`) reloads the snapshot and flushes the cache.

## Data model

See `internal/store/schema.sql` (version 1) and `internal/store/migrate_v2.sql`.
`store.Open` applies the migrations in order and tracks the version in
`PRAGMA user_version`; applied migrations are never edited.

- `zones`: name (lowercase FQDN with trailing dot), default TTL, serial.
  Overlapping zones (one inside another) are rejected so that every name maps
  to exactly one zone.
- `clusters`: id, `sha256(token)`, created_at, last_seen.
- `cluster_zones`: (cluster, zone) pairs; `UNIQUE(zone)`, so a zone belongs to
  at most one cluster. A cluster always keeps at least one zone.
- `records`: zone, name, type, data, ttl (0 = zone default), **owner**.
  `UNIQUE(name, type, data)`.
- `audit_log`: who (`admin` / `cluster:<id>`), what, when, JSON detail.

### Owners

| Owner | Written by | Shape |
|---|---|---|
| `manual` | admin token (CLI) | many records, any name in any zone |
| `<cluster>/<namespace>/<ingress>` | that cluster's token | the record set of one Ingress |

Rules, enforced inside the write transaction (single writer, so no races):

1. **First-wins:** a name is held by at most one owner. A write for a name held
   by someone else is skipped (owner PUT) or rejected with 409 (manual add).
2. **CNAME exclusivity:** a name with a CNAME has no other records; at most one
   CNAME per name; no CNAME at a zone apex.
3. Manual records can only be deleted by id through the admin API; operator
   records only disappear through their owner (PUT/DELETE), when the cluster
   is deleted, or when the zone they are in is taken away from the cluster.
4. A cluster token can only write records in its own zones, under its own
   owner prefix; the prefix comes from the token, never from the request.
   One owner (one Ingress) may hold names in several of the cluster's zones;
   each record goes to the cluster zone that contains its name, and a name
   outside all of them makes the whole PUT a 422.

### Owner PUT semantics

`PUT /v1/owners/{ns}/{name}/records` carries the complete desired set for that
owner. The server diffs it against what the owner currently has
(`internal/store/plan.go`), applies inserts/updates/deletes atomically, skips
conflicting names, and returns counts plus conflicts. Sending the same set
twice is a no-op. This keeps the operator stateless.

## Kubernetes operator

Separate Go module (`operator/`) so the server binary stays free of
client-go. Shares `pkg/api`, `pkg/client`, `pkg/ingressdns`.

- Startup: read the cluster token, `GET /v1/whoami` → cluster id + zones.
  An admin token, or a cluster without zones, is refused.
- Zones: re-read every `--zone-refresh` (1m), and immediately when horus
  answers a PUT with 422. When they change, every opted-in Ingress is queued
  again, so `horus cluster zone add|rm` needs no operator restart.
  `whoami` still returns `zone` (the first zone) for operators older than
  v0.2.0; a new operator falls back to it against an older server.
- Watch: Ingresses, filtered by a predicate that passes only Ingresses that
  are (or just stopped being) annotated, so unannotated Ingresses cause no
  API traffic.
- Reconcile:
  - Ingress gone, being deleted, or annotation removed → `DELETE owner`.
  - `pkg/ingressdns.Desired` builds the record set:
    hosts from `spec.rules[].host` (or `horus.warewave.tech/hostnames`),
    targets from `status.loadBalancer.ingress` (or `horus.warewave.tech/target`):
    IPs → A/AAAA (IPs win over hostnames), a hostname → CNAME.
    Wildcards, hosts outside the cluster's zones and apex CNAMEs are skipped
    with a Warning event.
  - No LB address yet → leave existing records alone, requeue in 30s.
  - `PUT owner` → events for conflicts (`HostnameConflict`) and changes
    (`Registered`). Conflicts are retried every 5 minutes.
  - Malformed annotations → Warning event, no change, no requeue.
- Garbage collection (leader only, every 5 min, uncached reads):
  `GET /v1/owners`, and delete owners whose Ingress is gone or opted out.
  Covers anything missed while the operator was down. No finalizers.

### Annotations

| Annotation | Meaning |
|---|---|
| `horus.warewave.tech/register: "true"` | opt in (required) |
| `horus.warewave.tech/ttl: "60"` | TTL for all records of the Ingress (1..86400) |
| `horus.warewave.tech/hostnames: "a.z,b.z"` | replace `spec.rules[].host` |
| `horus.warewave.tech/target: "10.0.0.5,2001:db8::5"` or `"lb.example.com"` | replace the LB status |

## Operations

- `make install` installs the binary, unit, env file and creates
  `/etc/horus/admin.token` (systemd credential). Then
  `systemctl enable --now horus horus-backup.timer`.
- Port 53: the unit grants `CAP_NET_BIND_SERVICE` to a `DynamicUser`.
  If systemd-resolved's stub listener holds `127.0.0.53:53`, either bind
  horus to a specific address (`HORUS_DNS_LISTEN=10.0.0.53:53`) or set
  `DNSStubListener=no`.
- API TLS: operators reach the API over `HORUS_API_LISTEN` (e.g. `:8053`);
  use TLS there. The service runs with `DynamicUser=yes`, so the cert and key
  are passed as systemd credentials: put them at `/etc/horus/tls/tls.{crt,key}`
  (key `0600 root`) and install `deploy/systemd/horus-tls.conf` as a drop-in
  (`make install` does this when the cert exists). horus uses
  `$CREDENTIALS_DIRECTORY/api-tls-{cert,key}` unless `HORUS_API_TLS_CERT/KEY`
  are set. The key pair is loaded before `READY=1`, so a bad cert fails the
  start. Credentials are copied at start: restart (not reload) after renewal.
  Operators that do not trust the issuing CA get it via `--ca-file`.
- Readiness: `READY=1` is sent only after both DNS sockets and the API are
  bound. Watchdog: 30s.
- Backups: `horus backup` → `VACUUM INTO` a timestamped file under
  `HORUS_BACKUP_DIR`; the timer keeps the 14 newest.
- Metrics: `GET /metrics` on the API listeners (no auth):
  `horus_dns_queries_total{result}`, `horus_upstream_errors_total{upstream}`,
  `horus_cache_entries`, `horus_zones`, `horus_records`.
- Logs: JSON on stderr → journald. `HORUS_QUERY_LOG=true` logs every query.

## Bootstrap example

```sh
horus zone add prod.internal.example
horus zone add staging.internal.example
horus record add db.prod.internal.example A 10.0.0.5
horus cluster add prod --zone prod.internal.example        # prints token once
horus zone add edge.internal.example
horus cluster zone add prod edge.internal.example          # prod may now write there too
kubectl create namespace horus-system
kubectl -n horus-system create secret generic horus-operator-token --from-literal=token=hrs_...
kubectl apply -f operator/config/operator.yaml   # after setting image + HORUS_URL
dig @horus-host web.prod.internal.example
```
