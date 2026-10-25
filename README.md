# horus

A small internal DNS server with a Kubernetes operator.

horus answers the names it holds records for, inside a few zones, and
forwards every other query to upstream resolvers through a TTL-aware cache.
That includes names inside those zones that horus does not hold, so a zone
can mix internal and public names. Data lives in SQLite.
You manage it with a REST API and a CLI, and it runs under systemd as one
static binary. In each Kubernetes cluster, an operator registers the hosts of
annotated Ingresses, so `web.prod.internal.example` resolves to the Ingress
load balancer a few seconds after the Ingress gets an address.

```
dig @horus web.prod.internal.example        → 10.20.0.15   (from an Ingress, via the operator)
dig @horus db.prod.internal.example         → 10.0.0.5     (added by hand with the CLI)
dig @horus github.com                       → forwarded to 1.1.1.1 / 9.9.9.9, cached
```

It is sized for a small team: about 20 people, a few clusters and hundreds of
records. Where simplicity and throughput conflicted, the design chose
simplicity.

## Contents

- [Features](#features)
- [Non-goals and limitations](#non-goals-and-limitations)
- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Installation](#installation)
- [First steps](#first-steps)
- [Configuration reference](#configuration-reference)
- [CLI reference](#cli-reference)
- [DNS behavior](#dns-behavior)
- [Records, zones and ownership](#records-zones-and-ownership)
- [Kubernetes operator](#kubernetes-operator)
- [REST API](#rest-api)
- [Security model](#security-model)
- [Operations](#operations)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [Repository layout](#repository-layout)
- [Versioning and releases](#versioning-and-releases)
- [License](#license)

## Features

- **Name overrides inside zones**: A, AAAA and CNAME records. horus answers
  the names it holds and forwards every other name, even inside its zones,
  so public records of the same domain keep working. CNAMEs are followed
  through names horus holds. A CNAME to any other name is resolved upstream
  and the result is appended, so stub resolvers get a complete answer.
- **Short negative caching**: names in our zones are never negatively cached
  by clients for more than 30 seconds. horus caps its own SOA and the SOA TTL
  of upstream negative answers for those names. A name that was just
  registered resolves quickly even for clients that cached its NXDOMAIN.
- **Forwarding**: an ordered list of upstreams with failover and
  "sticky last good" selection. Queries go over UDP first and retry over TCP
  when truncated.
- **Cache**: positive and negative caching (RFC 2308), with TTLs decremented
  on read. Identical concurrent queries share one upstream exchange
  (singleflight). `SIGHUP` flushes it.
- **Lock-free DNS hot path**: lookups read an immutable in-memory snapshot.
  Writes go to SQLite, then a new snapshot is swapped in atomically. When the
  API answers 2xx, the change is already being served.
- **REST API and CLI** for zones, records, clusters and backups. An admin
  token is used on a local unix socket, and per-cluster tokens over TCP (TLS).
- **Kubernetes operator**: one per cluster, leader-elected. Annotate an
  Ingress and its hosts are registered. Supports multiple clusters, several
  zones per cluster (added or removed at runtime, no operator restart),
  first-wins name ownership, and garbage collection of anything missed while
  the operator was down.
- **Operations**: systemd `Type=notify` with watchdog, `DynamicUser` and a
  strict sandbox. Online backups (`VACUUM INTO`) with a daily timer.
  Prometheus metrics, JSON logs to journald, an audit log of every write.
- **Few dependencies**: [`miekg/dns`](https://github.com/miekg/dns) and the
  cgo-free [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) for
  the server. Metrics and sd_notify are small stdlib implementations. The
  operator is a separate Go module, so the server binary never links
  client-go.

## Non-goals and limitations

These are deliberate decisions (see the "Decisions" table in
[docs/DESIGN.md](docs/DESIGN.md)):

| Not supported | Why / what to do instead |
|---|---|
| Record types other than A, AAAA, CNAME (MX, TXT, SRV, NS, PTR, …) | Not needed for service discovery. Queries for other types get NODATA. |
| Wildcard records (`*.zone`) | Ambiguous with first-wins ownership. Wildcard Ingress hosts are skipped with an event. |
| DNSSEC | Internal names only. Upstream queries are sent without the DO bit. |
| High availability, replication | One node, `Restart=on-failure`, daily backups. Point clients at a second resolver as a fallback if you need one. |
| Recursive resolution | horus forwards to recursive resolvers; it does not walk the DNS tree itself. |
| Zone transfers (AXFR/IXFR), NS records, delegation | horus does not serve NS records at the apex, so its zones cannot be delegated from a parent zone. Clients must use horus as their resolver (directly, or through a conditional forwarder). |
| Overlapping zones (`a.example` and `b.a.example`) | Rejected, so every name maps to exactly one zone and one owner. |
| One zone shared by several clusters | A zone belongs to at most one cluster (a cluster may have many zones). Give each cluster its own zones; cross-cluster names can still be joined with manual CNAMEs. |

## How it works

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

- **Read path**: `dnsserver.Handler` asks the current `zone.Snapshot`. If
  horus holds no record for the name (outside every zone, or a name inside a
  zone that horus does not hold), the query is forwarded. Otherwise the
  snapshot answers on its own. It never touches SQLite and never takes a lock.
- **Write path**: every API write takes a mutex, runs one SQLite transaction
  (which also bumps the zone serial and appends to `audit_log`), then
  rebuilds the snapshot from the database and swaps it in. Operator writes
  that change nothing skip the rebuild.
- **Operator**: watches annotated Ingresses. On each reconcile it sends the
  complete desired record set for that Ingress (`PUT`). The server diffs that
  set against what the Ingress currently owns and applies the difference.
  This keeps the operator stateless.

The full design (decisions, rules, data model) is in
[docs/DESIGN.md](docs/DESIGN.md). The REST contract is in
[docs/API.md](docs/API.md).

## Requirements

| Component | Needs |
|---|---|
| Building | Go 1.26 or newer. No cgo, no C toolchain. |
| Server | Linux with systemd (any recent distribution). Credentials need systemd ≥ 247 (`LoadCredential=`, `$CREDENTIALS_DIRECTORY`). `DynamicUser=` needs ≥ 235. |
| Server (without systemd) | Any OS Go supports. You manage the process, the token file and port 53 privileges yourself. |
| Operator | Kubernetes with `networking.k8s.io/v1` Ingress (1.19+). An ingress controller that fills `status.loadBalancer` (or use the `target` annotation). |
| e2e tests | docker, kind, kubectl ≥ 1.24 (`--subresource`), dig, openssl, curl, perl |

## Installation

### 1. Build

```sh
git clone git@github.com:Warewave-Technology/horus.git
cd horus
make build            # → bin/horus (static, CGO_ENABLED=0)
make operator         # → bin/horus-operator
make operator-image   # → docker image horus-operator:<version>
```

`make` with no target runs `vet test build operator`.

The version embedded in the binary (`horus version`) comes from
`git describe --tags --always --dirty`. Override it with
`make build VERSION=1.2.3`.

To cross-compile for the server (for example, building on a Mac for a Linux
host):

```sh
GOOS=linux GOARCH=amd64 make build
```

Prebuilt Linux binaries are attached to the
[GitHub releases](https://github.com/Warewave-Technology/horus/releases).

### 2. Install the server (systemd)

On the DNS host, as root:

```sh
make install
systemctl enable --now horus horus-backup.timer
```

`make install` does the following (it is idempotent and never overwrites
your config or token):

| Step | Result |
|---|---|
| Binary | `/usr/local/bin/horus` |
| Config | `/etc/horus/horus.env`, copied from [`deploy/systemd/horus.env.example`](deploy/systemd/horus.env.example) only if missing |
| Admin token | `/etc/horus/admin.token` (mode `0600`, generated with `horus token`) only if missing |
| Units | `/etc/systemd/system/horus.service`, `horus-backup.service`, `horus-backup.timer` |
| TLS drop-in | `/etc/systemd/system/horus.service.d/tls.conf`, only if `/etc/horus/tls/tls.crt` exists (see [API TLS](#api-tls)) |
| `systemctl daemon-reload` | |

The unit runs as an ephemeral unprivileged user (`DynamicUser=yes`) with
only `CAP_NET_BIND_SERVICE`, so it can bind port 53 and nothing more.

> **Port 53 and systemd-resolved.** If `systemd-resolved` runs its stub
> listener on `127.0.0.53:53`, binding `:53` fails with "address already in
> use". Either bind horus to a specific address
> (`HORUS_DNS_LISTEN=10.0.0.53:53`) or set `DNSStubListener=no` in
> `/etc/systemd/resolved.conf` and restart `systemd-resolved`.

### 3. Without systemd

horus does not need systemd. Without `$NOTIFY_SOCKET` it skips readiness
and watchdog notifications. Give it a token file and paths it can write:

```sh
horus token > admin.token && chmod 600 admin.token
HORUS_ADMIN_TOKEN_FILE=$PWD/admin.token \
HORUS_DB=$PWD/horus.db \
HORUS_BACKUP_DIR=$PWD/backups \
HORUS_API_SOCKET=$PWD/api.sock \
HORUS_DNS_LISTEN=127.0.0.1:15353 \
horus serve
```

Port 53 then needs root or `setcap cap_net_bind_service=+ep /usr/local/bin/horus`.

## First steps

```sh
# Zones (each cluster gets one or more, plus any you manage by hand)
horus zone add prod.internal.example
horus zone add edge.internal.example
horus zone add staging.internal.example --ttl 60

# Manual records
horus record add db.prod.internal.example A 10.0.0.5
horus record add db.prod.internal.example AAAA 2001:db8::5
horus record add pg.prod.internal.example CNAME db.prod.internal.example

dig @127.0.0.1 pg.prod.internal.example
#   pg.prod.internal.example.  300  IN  CNAME  db.prod.internal.example.
#   db.prod.internal.example.  300  IN  A      10.0.0.5

# A token for a cluster's operator (printed once; store it in a Secret).
# The cluster may register names in all of its zones.
horus cluster add prod --zone prod.internal.example --zone edge.internal.example

# Zones can be given to or taken from a cluster later; the operator follows
# within a minute, without a restart.
horus cluster zone add prod staging.internal.example
horus cluster zone rm prod staging.internal.example
```

Then [deploy the operator](#deploying-the-operator) into that cluster and
annotate an Ingress with `horus.warewave.tech/register: "true"`.

Finally, point your clients (DHCP, `/etc/resolv.conf`, VPN config, or a
conditional forwarder on your existing resolver) at the horus host.

## Configuration reference

The server is configured only through `HORUS_*` environment variables. Under
systemd they come from `/etc/horus/horus.env` (`EnvironmentFile=`). Every
variable has a default. An empty value counts as unset. Invalid values stop
the start with a message that lists every problem at once.

### Server (`horus serve`)

| Variable | Default | Meaning |
|---|---|---|
| `HORUS_DNS_LISTEN` | `:53` | DNS listen address, UDP and TCP. |
| `HORUS_API_SOCKET` | `/run/horus/api.sock` | Unix socket for the local CLI, mode `0660`. `none` disables it. A stale socket from a previous run is removed at start. The socket is removed on clean shutdown. |
| `HORUS_API_LISTEN` | *(disabled)* | TCP listener for operators, for example `:8053`. Use TLS. Without TLS a warning is logged at start. At least one of socket or TCP listener is required. |
| `HORUS_API_TLS_CERT` | *(see below)* | PEM certificate (chain) for `HORUS_API_LISTEN`. |
| `HORUS_API_TLS_KEY` | *(see below)* | PEM private key. Set both or neither. If neither is set, horus uses the systemd credentials `api-tls-cert` and `api-tls-key` when they exist. The key pair is loaded before startup completes, so a bad certificate fails the start. Minimum TLS 1.2. |
| `HORUS_ADMIN_TOKEN_FILE` | `$CREDENTIALS_DIRECTORY/admin-token` | File holding the admin token (surrounding whitespace is trimmed). At least 32 characters. Required. |
| `HORUS_DB` | `/var/lib/horus/horus.db` | SQLite database. The directory is created (`0700`). WAL mode, so `horus.db-wal` and `horus.db-shm` live next to it. |
| `HORUS_BACKUP_DIR` | `/var/lib/horus/backups` | Where `horus backup` writes on the server. Created on demand (`0700`). |
| `HORUS_UPSTREAMS` | `1.1.1.1,9.9.9.9` | Comma-separated upstream resolvers, tried in order. The port defaults to 53. IPv6 works bare (`2606:4700::1111`) or bracketed with a port (`[2606:4700::1111]:53`). |
| `HORUS_UPSTREAM_TIMEOUT` | `2s` | Per-exchange timeout (Go duration) for each upstream attempt. |
| `HORUS_CACHE_MAX_ENTRIES` | `10000` | Forwarding cache size. When full, expired entries are evicted first, then the entry closest to expiry. |
| `HORUS_CACHE_MAX_TTL` | `1h` | Cap for caching positive upstream answers. |
| `HORUS_CACHE_NEG_TTL` | `5m` | Cap for caching NXDOMAIN/NODATA upstream answers. |
| `HORUS_SOA_MNAME` | `ns.horus.` | Primary name server field of the synthesized SOA. Must end with a dot. |
| `HORUS_SOA_RNAME` | `hostmaster.horus.` | Responsible mailbox field of the SOA. Must end with a dot. |
| `HORUS_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `HORUS_QUERY_LOG` | `false` | Log every DNS query at info level (client, qname, qtype, rcode, result, duration). Verbose. |

systemd credentials (from `LoadCredential=`, read from
`$CREDENTIALS_DIRECTORY`):

| Credential | Used for | Provided by |
|---|---|---|
| `admin-token` | admin token, unless `HORUS_ADMIN_TOKEN_FILE` is set | `horus.service` (`/etc/horus/admin.token`) |
| `api-tls-cert` | TLS certificate, unless `HORUS_API_TLS_*` is set | `horus-tls.conf` drop-in (`/etc/horus/tls/tls.crt`) |
| `api-tls-key` | TLS key | `horus-tls.conf` drop-in (`/etc/horus/tls/tls.key`) |

### CLI

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `--addr` | `HORUS_ADDR` | `unix:///run/horus/api.sock` | API address: `unix:///path`, `http://host:port` or `https://host:port`. |
| `--token-file` | `HORUS_TOKEN_FILE` | `/etc/horus/admin.token` | Token file. |
| | `HORUS_TOKEN` | | The token itself. Overrides the file. |
| `--json` | | `false` | Machine-readable output. |

Global flags go **before** the command (`horus --json record ls`). Each API
request times out after 10 seconds.

### Operator

See [Operator flags](#operator-flags).

## CLI reference

The `horus` binary is both the server and the admin client. All commands
except `serve`, `token`, `version` and `help` talk to the API, by default
over the local unix socket. That socket is only accessible to root, so run
them as root (or with `sudo`).

```
horus serve                                   run the DNS + API server
horus token                                   print a new random admin token (48 chars)
horus version                                 print the version
horus help                                    usage

horus zone ls
horus zone add <zone> [--ttl N]               default TTL for records without their own (default 300, max 86400)
horus zone rm <zone>                          deletes the zone and all its records; refused while a cluster is bound to it

horus record ls [zone] [--owner OWNER]        all zones if none given
horus record add <name> <A|AAAA|CNAME> <data> [--ttl N]
horus record rm <id>                          manual records only (ids from `record ls`)

horus owners [zone] [--owner OWNER]           who holds which names: owner, record count, names, last change

horus cluster ls                              id, zones, created, last seen (precision: 1 minute)
horus cluster add <id> --zone <zone> [--zone <zone>...]
                                              prints the cluster token once; --zone a,b also works
horus cluster zone add <id> <zone>            let the cluster write to one more zone
horus cluster zone rm <id> <zone>             take a zone away; deletes the cluster's records in it
horus cluster rotate <id>                     prints a new token; the old one stops working immediately
horus cluster rm <id>                         deletes the cluster and every record it owns

horus backup                                  consistent DB copy on the server; prints its path
horus whoami                                  "admin" or "cluster <id> (zones <zone>, ...)"
```

Details:

- Names may be given with or without the trailing dot, in any case. They are
  stored lowercase with a trailing dot.
- `record add` picks the zone automatically: the existing zone that contains
  the name.
- `--ttl 0` (or no `--ttl`) means "use the zone default". The zone default is
  applied when serving, so changing it later affects these records.
- `OWNER` filters accept `manual`, a cluster id (`prod`), a cluster and
  namespace (`prod/default`), or a full owner (`prod/default/web`). `prod`
  does not match `production/...`.
- Flags may appear anywhere after the subcommand (`horus record add --ttl 60 a.z A 10.0.0.1`).
- Exit code is 0 on success, 1 on any error. Errors are printed as
  `horus: <status>: <message>`. First-wins conflicts are printed as
  `horus: conflict (first-wins): <name> is owned by <owner>`.

Example output:

```
$ horus owners
OWNER             RECORDS  NAMES                                                            UPDATED
manual            7        a.b.prod.internal.example.,db.prod.internal.example.,... (+5)    2026-10-20 19:22
prod/default/web  1        web.prod.internal.example.                                       2026-10-20 19:28

$ horus record ls --owner prod
ID  NAME                        TYPE  DATA        TTL      OWNER             UPDATED
8   web.prod.internal.example.  A     10.20.0.15  default  prod/default/web  2026-10-20 19:28
```

## DNS behavior

### Request handling

| Request | Response |
|---|---|
| Opcode other than QUERY | `NOTIMP` |
| Not exactly one question | `FORMERR` |
| EDNS version other than 0 | `BADVERS` |
| Class other than IN (for example `CH TXT version.bind`) | `REFUSED` |
| Name outside every zone | forwarded (see below) |
| Name inside a zone | answered from the snapshot, `AA` set |

All responses set `RA`. The client's question section is echoed with its
original casing (0x20 randomization works). Owner names in answers are
lowercase.

### Which names horus answers

horus **overrides names, it does not own zones**. A zone says where horus may
hold names. Any name horus holds no record for is forwarded upstream, even
inside the zone. You can therefore make `example.com` a zone, add
`internal.example.com`, and `www.example.com` still resolves from public DNS.

| Query | Response |
|---|---|
| Name outside every zone | forwarded |
| Name inside a zone, horus holds no record for it (unknown name, empty non-terminal, zone apex without records) | forwarded; for NXDOMAIN/NODATA the SOA TTL is capped at 30s (see below) |
| Name horus holds, matching type | the records, `AA` set |
| Name horus holds, other type (for example only A, query AAAA) | NODATA with horus's SOA, `AA` set, **not** forwarded |
| Name horus holds with a CNAME | the CNAME, followed (see below) |

Why a held name is never partly forwarded: if horus holds an internal A for a
name and the public zone has an AAAA (or a CNAME) for it, merging the two
would send IPv6-preferring clients to the public endpoint. A name horus holds
is answered by horus alone. Add the AAAA to horus too if you need it.

Details:

1. **Exact match** of the query type → the records (`NOERROR`). Several A or
   AAAA records for a name are all returned. `ANY` returns every record of
   the name.
2. **CNAME** (and the query is not for CNAME) → the CNAME is returned and
   followed:
   - to a name horus holds (in any of its zones): the target's records are
     appended. Chains are followed for at most 8 CNAMEs. A loop
     (`a → b → a`) stops after returning each CNAME once.
   - to any other name (outside our zones, or inside them but not held): the
     target is resolved upstream (through the cache) and its answer appended.
     If upstream says NXDOMAIN, the response is NXDOMAIN, with the upstream
     SOA (TTL capped). If upstream fails, the client still gets the CNAME and
     can chase it itself.
3. **NODATA** answers carry the synthesized SOA of the zone that holds the
   last name of the chain.
4. **SOA and NS queries** are forwarded unless the apex is a name horus
   holds, in which case they get NODATA (horus serves no NS records).

### Synthesized SOA

Used in horus's own NODATA answers:

| Field | Value |
|---|---|
| Owner, TTL | zone apex, zone default TTL |
| MNAME / RNAME | `HORUS_SOA_MNAME` / `HORUS_SOA_RNAME` |
| SERIAL | starts at 1, +1 for every write that touches the zone |
| REFRESH / RETRY / EXPIRE | 3600 / 600 / 86400 (informational; there are no secondaries) |
| MINIMUM | `min(zone default TTL, 30)` |

### Negative caching of names in our zones

Clients cache a negative answer for `min(SOA TTL, SOA MINIMUM)` (RFC 2308).
For every name inside one of our zones, horus keeps that at 30 seconds or
less:

- its own NODATA answers use MINIMUM ≤ 30;
- upstream NXDOMAIN/NODATA answers for names in our zones (forwarded, or at
  the end of a chased CNAME) get their SOA **TTL** lowered to 30. The SOA
  data itself is not changed.

That is the longest a client keeps seeing NXDOMAIN for a name that was just
registered (by the operator or with `horus record add`). horus's own
forwarding cache does not delay it either: names horus holds are always
answered from the snapshot first.

Positive public answers are not capped. If a name already resolves publicly
and you then add it to horus, clients that cached the public answer keep it
until its TTL runs out.

### EDNS and truncation

- horus advertises a 1232-byte EDNS buffer (DNS Flag Day 2020) and answers
  EDNS queries with an OPT record. The DO bit is echoed, but no DNSSEC
  records are ever produced.
- UDP responses are truncated (`TC` set) to the client's advertised size,
  clamped to 512–1232 bytes, or 512 without EDNS. Clients then retry over
  TCP. TCP connections time out after 5 seconds idle.

### Forwarding

- Upstreams are tried **in order, starting from the one that answered last**.
  A failed exchange (timeout, network error) or an rcode other than
  `NOERROR`/`NXDOMAIN` (for example `SERVFAIL`, `REFUSED`) moves on to the
  next upstream. If every upstream fails, the client gets `SERVFAIL`.
- Queries go out with `RD` set and EDNS 1232 without DO, over UDP first. On
  truncation, the same upstream is asked again over TCP.
- Upstream answers are relayed with `AA` cleared.

### Cache

- Key: lowercased name + type + class.
- Positive answers are cached for the lowest TTL in the message (OPT
  excluded), capped by `HORUS_CACHE_MAX_TTL`.
- Negative answers (NXDOMAIN, or NOERROR without answers) are cached for
  `min(SOA TTL, SOA MINIMUM)` from the authority section, capped by
  `HORUS_CACHE_NEG_TTL`. Without an SOA they are not cached.
- Truncated answers and TTL 0 answers are not cached.
- TTLs are decremented on read, so clients see the remaining lifetime.
- Concurrent identical misses share one upstream exchange.
- Expired entries are swept every minute. `systemctl reload horus` (SIGHUP)
  flushes the whole cache.
- Names horus holds are never cached. They are answered from the snapshot,
  which is checked before the cache. Other names inside our zones are
  forwarded and cached like any other name.

## Records, zones and ownership

### Validation

| Rule | Detail |
|---|---|
| Names | Lowercase FQDN with a trailing dot after normalization, at most 253 characters. Labels contain `a-z 0-9 _ -`, up to 63 characters, and do not start or end with `-`. `*` is rejected. |
| In zone | A record's name must be inside the zone it is added to. |
| A | IPv4 address, stored in canonical form. |
| AAAA | IPv6 address, stored in canonical form. IPv4-mapped (`::ffff:1.2.3.4`) and zoned (`fe80::1%eth0`) addresses are rejected. |
| CNAME | The target is a valid name, not the record's own name. Not allowed at the zone apex. A name with a CNAME has no other records, and at most one CNAME. |
| TTL | 0..86400. 0 = zone default. |
| Uniqueness | `(name, type, data)` is unique across the whole server. |
| Zones | No zone may be inside another. Zone default TTL 1..86400 (0 on create = 300). |

Validation errors are `422` from the API and are printed by the CLI.

### Owners and first-wins

Every record has an **owner**:

| Owner | Created by | Can be removed by |
|---|---|---|
| `manual` | admin token (CLI) | `horus record rm <id>`, `horus zone rm` |
| `<cluster>/<namespace>/<ingress>` | that cluster's operator | the operator (Ingress deleted/changed/opted out), garbage collection, `horus cluster rm`, `horus cluster zone rm` (records in that zone) |

Rules, enforced inside the write transaction:

1. **First-wins**: a name belongs to at most one owner. Whoever registers a
   name first keeps it until they release it.
   - An operator registering a name held by someone else: that name is
     **skipped and reported** (`HostnameConflict` event). The other names of
     the Ingress are still registered. The operator retries every 5 minutes,
     so the name is picked up automatically once it is released.
   - `horus record add` for a name held by an operator: rejected with `409`.
2. **CNAME exclusivity** (see the table above) holds across all records of a
   name.
3. **Manual records cannot be deleted by operators**, and operator records
   cannot be deleted with `horus record rm` (`409`).
4. **A cluster token can only write in its own zones** and under its own
   owner prefix. The prefix comes from the token, never from the request.
   One Ingress may hold names in several of the cluster's zones. Each record
   is stored in the cluster zone that contains it.
5. **A zone belongs to at most one cluster**, and a cluster keeps at least one
   zone. A zone that belongs to a cluster cannot be deleted.

### Audit log

Every write appends a row to the `audit_log` table: timestamp, actor
(`admin` or `cluster:<id>`), action (`zone.create`, `zone.delete`,
`record.create`, `record.delete`, `owner.replace`, `owner.delete`,
`cluster.create`, `cluster.zone.add`, `cluster.zone.remove`, `cluster.rotate`,
`cluster.delete`) and a JSON detail.
Operator reconciles that change nothing are not logged, unless they report
conflicts (then one row per retry, every 5 minutes). There is no CLI for
it yet. Read it with `sqlite3`, which is safe while horus runs because of WAL:

```sh
sqlite3 -readonly /var/lib/horus/horus.db \
  "select datetime(ts,'unixepoch'), actor, action, target from audit_log order by id desc limit 20"
```

## Kubernetes operator

One operator runs per cluster. It authenticates with that cluster's token
and learns its cluster id and zones from `GET /v1/whoami` at startup.

The zones are read again every minute (`--zone-refresh`), and immediately
when horus rejects a record set. When they change, every opted-in Ingress is
reconciled again. Hosts in a newly added zone get registered, and hosts in a
removed zone are dropped. `horus cluster zone add|rm` therefore takes effect
within a minute, without restarting the operator.

### Annotations

| Annotation | Value | Meaning |
|---|---|---|
| `horus.warewave.tech/register` | `"true"` | **Required.** Opt the Ingress in (case-insensitive). Any other value, or removing it, unregisters the Ingress. |
| `horus.warewave.tech/ttl` | `"1"`..`"86400"` | TTL for all records of the Ingress. Default: the zone default TTL. |
| `horus.warewave.tech/hostnames` | `"a.zone,b.zone"` | Use these names instead of `spec.rules[].host`. |
| `horus.warewave.tech/target` | `"10.0.0.5,2001:db8::5"` or `"lb.example.com"` | Use these targets instead of `status.loadBalancer`. Either IPs (any mix of IPv4/IPv6 → A/AAAA) or exactly one hostname (→ CNAME). Mixing IPs and hostnames is an error. |

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: web
  namespace: default
  annotations:
    horus.warewave.tech/register: "true"
    horus.warewave.tech/ttl: "60"                     # optional
spec:
  ingressClassName: nginx
  rules:
    - host: web.prod.internal.example
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service: { name: web, port: { number: 80 } }
```

Result, once the ingress controller publishes an address:
`web.prod.internal.example. 60 IN A <status.loadBalancer.ingress[].ip>`.
More examples are in
[`operator/config/example-ingress.yaml`](operator/config/example-ingress.yaml).

### How an Ingress becomes records

- **Names**: `spec.rules[].host` (or the `hostnames` annotation), lowercased
  and de-duplicated. Rules without a host are ignored. Names are skipped,
  with a `HostSkipped` warning event, when:
  - they are wildcards (`*.zone`)
  - they are outside all of the cluster's zones
  - they are the zone apex and the target would be a CNAME
- **Targets**: `status.loadBalancer.ingress[]` (or the `target` annotation).
  - IPs become A/AAAA records (IPv4-mapped IPv6 is unmapped). Several IPs
    give several records per name.
  - If the status has both IPs and hostnames, **IPs win**, because a CNAME
    cannot coexist with A/AAAA.
  - Hostnames become one CNAME. If there are several, the alphabetically
    first is used.
- **No address yet** (empty LB status, no `target` annotation): existing
  records are **left alone** and the Ingress is re-checked every 30 seconds
  (`--pending-requeue`). Status updates also trigger a reconcile.
- Each reconcile sends the complete desired set. The server adds, updates
  (TTL) and removes records so the owner `<cluster>/<namespace>/<name>`
  holds exactly that set.

### Events

| Reason | Type | When |
|---|---|---|
| `Registered` | Normal | Records were added, updated or removed (with counts). |
| `HostnameConflict` | Warning | A name is held by another owner. It is retried every 5 minutes. |
| `HostSkipped` | Warning | A host was skipped (wildcard, outside the cluster's zones, CNAME at the apex). |
| `InvalidAnnotation` | Warning | An annotation is malformed. Nothing changes on the server and there is no retry. Fixing the Ingress triggers a new reconcile. |
| `Rejected` | Warning | The server refused the record set (`422`) and the cluster's zones did not change. No retry. (If the zones did change, for example a zone was just removed, there is no event: all Ingresses are reconciled again with the new zones.) |

```sh
kubectl describe ingress web          # events at the bottom
kubectl get events --field-selector reason=HostnameConflict -A
```

### Removal and garbage collection

- Records are removed when the Ingress is deleted, is being deleted, or
  stops being opted in. There are no finalizers: deleting an Ingress never
  blocks on horus.
- **Garbage collection** runs on the leader at startup and then every
  5 minutes (`--gc-interval`). It lists the cluster's owners on the server,
  reads each Ingress directly from the API server (not from the cache), and
  deletes owners whose Ingress is gone or opted out. This covers deletes that
  happened while the operator was down.
- Unannotated Ingresses are filtered out before reconciling, so they cause no
  traffic to horus.
- Transient API errors (network, `5xx`) are retried with exponential backoff.

### Deploying the operator

On the horus host:

```sh
horus cluster add prod --zone prod.internal.example     # copy the token (hrs_...)
```

In the cluster:

```sh
kubectl create namespace horus-system
kubectl -n horus-system create secret generic horus-operator-token \
  --from-literal=token=hrs_...
# If the horus API certificate is not signed by a CA in the image's trust store:
#   add  --from-file=ca.crt=ca.pem  to the command above, and
#   add  --ca-file=/var/run/secrets/horus/ca.crt  to the container args.
```

The image is published for `linux/amd64` and `linux/arm64`:

```
ghcr.io/warewave-technology/horus-operator:v0.2.0     # a release (pin this)
ghcr.io/warewave-technology/horus-operator:latest     # newest release
```

To build your own (from the repository root, because the operator module
imports `../pkg`):

```sh
make operator-image VERSION=dev                                       # local, current platform
make operator-push VERSION=v0.1.1 IMAGE=registry.example.com/horus-operator   # multi-arch, pushed
```

Edit [`operator/config/operator.yaml`](operator/config/operator.yaml): set
the `image` and `HORUS_URL` (marked `CHANGE ME`). Then:

```sh
kubectl apply -f operator/config/operator.yaml
kubectl -n horus-system rollout status deploy/horus-operator
kubectl -n horus-system logs deploy/horus-operator
```

The manifest contains:

- the `horus-system` namespace and a ServiceAccount;
- a ClusterRole with `get/list/watch` on Ingresses and `create/patch` on
  Events;
- a Role for the leader election Lease (`horus-operator.warewave.tech`);
- a Deployment with one replica (more are fine with leader election). It runs
  as non-root with a read-only root filesystem, all capabilities dropped and
  the RuntimeDefault seccomp profile. Requests are 10m CPU and 32Mi memory;
  the limit is 128Mi.

The image is `distroless/static:nonroot` with a single static binary.

### Operator flags

| Flag | Default | Meaning |
|---|---|---|
| `--horus-url` | `$HORUS_URL` | horus API base URL, for example `https://horus.internal.example:8053`. Required. |
| `--token-file` | `/var/run/secrets/horus/token` | Cluster token. Read once at startup. |
| `--ca-file` | | PEM CA bundle to verify the horus certificate. Default: system roots. |
| `--leader-elect` | `true` | Leader election (needed with more than one replica). |
| `--gc-interval` | `5m` | Garbage collection interval. |
| `--pending-requeue` | `30s` | Re-check interval for Ingresses without an LB address. |
| `--zone-refresh` | `1m` | How often to re-read the cluster's zones from horus. |
| `--metrics-bind-address` | `:8080` | controller-runtime metrics. `0` disables. |
| `--health-probe-bind-address` | `:8081` | `/healthz` (process alive) and `/readyz` (horus reachable: calls `whoami`). |
| `--zap-*` | | Logging flags (`--zap-log-level=debug`, `--zap-devel`, …). |

Startup fails fast, and Kubernetes restarts the pod, when:
- horus is unreachable or the token is invalid (`whoami`, 15s timeout);
- the token is an admin token. The operator refuses it: it needs a cluster
  token;
- the cluster has no zones.

Compatibility: an operator from v0.2.0 or later also works against an older
server (it then uses the single `zone` from `whoami`). An older operator
works against a newer server, but only registers names in the cluster's
first zone (alphabetical). Upgrade the server first, then the operators.

## REST API

The full contract is in [docs/API.md](docs/API.md). Summary:

| Method | Path | Token |
|---|---|---|
| GET | `/healthz`, `/metrics` | none |
| GET | `/v1/whoami` | any |
| GET, POST | `/v1/zones` | admin |
| DELETE | `/v1/zones/{zone}` | admin |
| GET, POST | `/v1/zones/{zone}/records` | admin |
| DELETE | `/v1/records/{id}` | admin |
| GET, POST | `/v1/clusters` | admin |
| POST | `/v1/clusters/{id}/zones` | admin |
| DELETE | `/v1/clusters/{id}/zones/{zone}` | admin |
| POST | `/v1/clusters/{id}/rotate-token` | admin |
| DELETE | `/v1/clusters/{id}` | admin |
| POST | `/v1/backup` | admin |
| GET | `/v1/owners` | cluster |
| PUT | `/v1/owners/{namespace}/{name}/records` | cluster |
| DELETE | `/v1/owners/{namespace}/{name}` | cluster |

```sh
curl -s --unix-socket /run/horus/api.sock \
  -H "Authorization: Bearer $(cat /etc/horus/admin.token)" http://horus/v1/zones
```

Status codes: `401` missing/unknown token, `403` wrong token kind, `404` not
found, `409` exists or first-wins conflict, `422` validation, `500` internal.
Any `2xx` on a write means the change is already live in DNS.

The Go client used by the CLI and the operator is
[`pkg/client`](pkg/client/client.go) (stdlib only).

## Security model

- **Admin token**: one, in `/etc/horus/admin.token` (`0600 root`). It is
  passed to the service as a systemd credential, so the service user never
  needs to read `/etc/horus`. It is compared in constant time. Only root on
  the host can reach the default unix socket (`/run/horus` is `0750`, the
  socket `0660`, both owned by the dynamic service user).
- **Cluster tokens**: `hrs_` + 32 random bytes (base64url). They are shown
  once and stored only as SHA-256. Each token is bound to one cluster id and
  that cluster's zones. A leaked cluster token can change only that cluster's
  records in those zones, never manual records or names held by other owners.
  Zones are assigned only with the admin token.
- **Transport**: the operator API sends bearer tokens. Use TLS on
  `HORUS_API_LISTEN`. horus warns at start when the TCP listener has no TLS.
- **Unauthenticated endpoints**: `/healthz` and `/metrics`, on every API
  listener. They expose counts, not names.
- **DNS**: horus answers anyone who can reach port 53, and forwards for them.
  Do not expose it to the internet (it would be an open resolver). Restrict
  access with your firewall.
- **Sandbox**: `DynamicUser`, only `CAP_NET_BIND_SERVICE`,
  `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `PrivateDevices`,
  kernel/cgroup/clock/hostname protection, `RestrictAddressFamilies=AF_INET
  AF_INET6 AF_UNIX`, `MemoryDenyWriteExecute`, `SystemCallFilter=@system-service`,
  `UMask=0077`. See [`deploy/systemd/horus.service`](deploy/systemd/horus.service).
- **Request limits**: JSON bodies are limited to 1 MiB, and unknown fields are
  rejected. HTTP read-header timeout 5s, read/write 30s, idle 2m.

## Operations

### Service management

```sh
systemctl status horus           # STATUS= line shows "serving N zones, M records"
systemctl reload horus           # SIGHUP: reload the snapshot from the DB and flush the forwarding cache
systemctl restart horus          # needed after changing horus.env, tokens or TLS files
journalctl -u horus -f           # logs (JSON)
```

- `Type=notify`: `READY=1` is sent only after both DNS sockets and the API
  listeners are bound and the snapshot is loaded. Units ordered after
  `horus.service` start once it really serves.
- Watchdog: `WatchdogSec=30s`. horus pings every 15 seconds. A hung process
  is killed and restarted (`Restart=on-failure`, after 2 seconds).
- Shutdown: on SIGTERM, horus finishes in-flight requests (5s budget) and
  removes the socket.

### API TLS

The unit uses `DynamicUser=yes`, so the service cannot read root-owned key
files. TLS material is therefore passed as systemd credentials:

```sh
install -d -m 0755 /etc/horus/tls
install -m 0644 tls.crt /etc/horus/tls/tls.crt      # certificate (+ chain)
install -m 0600 tls.key /etc/horus/tls/tls.key      # private key, stays root-only
install -d /etc/systemd/system/horus.service.d
install -m 0644 deploy/systemd/horus-tls.conf /etc/systemd/system/horus.service.d/tls.conf
echo 'HORUS_API_LISTEN=:8053' >> /etc/horus/horus.env
systemctl daemon-reload && systemctl restart horus
```

(`make install` installs the drop-in for you if `/etc/horus/tls/tls.crt`
exists.)

- The certificate needs the name the operators use in `HORUS_URL` as a SAN.
- Credentials are copied when the service starts. **After renewing the
  certificate, `systemctl restart horus`**. `reload` is not enough.
- With a private CA, give the CA to the operators (`--ca-file`).
- Check it: `curl --cacert ca.pem https://horus.internal.example:8053/healthz`.

### Backups and restore

`horus-backup.timer` runs `horus backup` daily (randomized by up to
30 minutes, catching up after downtime). The server writes a consistent copy
with `VACUUM INTO` to `HORUS_BACKUP_DIR/horus-<UTC timestamp>.db`. The
service then keeps the 14 newest files. Manual backup:

```sh
horus backup        # → /var/lib/horus/backups/horus-20261020T120000Z.db
```

Copy backups off the host (rsync, restic, …). They sit on the same disk as
the database.

Restore:

```sh
systemctl stop horus
cp /var/lib/horus/backups/horus-20261020T120000Z.db /var/lib/horus/horus.db
rm -f /var/lib/horus/horus.db-wal /var/lib/horus/horus.db-shm
chown --reference=/var/lib/private/horus /var/lib/horus/horus.db
chmod 0600 /var/lib/horus/horus.db
systemctl start horus
horus zone ls && horus record ls
```

With `DynamicUser=yes`, `/var/lib/horus` is a symlink to
`/var/lib/private/horus`, owned by the service's dynamic UID. A file copied in
as root is not readable by the service. Give it the directory's owner before
starting, as shown above.

Operator-owned records come back by themselves anyway: the next reconcile or
garbage collection restores each cluster's records. To force it, restart the
operator.

### Token rotation

- **Cluster token**:
  ```sh
  horus cluster rotate prod                       # old token stops working immediately
  kubectl -n horus-system create secret generic horus-operator-token \
    --from-literal=token=hrs_NEW --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n horus-system rollout restart deploy/horus-operator   # token is read at startup
  ```
  Records are not affected. Between the rotation and the restart, the
  operator's calls fail with `401` and are retried.
- **Admin token**:
  ```sh
  (umask 077; horus token > /etc/horus/admin.token)
  systemctl restart horus
  ```

### Cluster zones

```sh
horus zone add edge.internal.example
horus cluster zone add prod edge.internal.example    # prod's operator registers edge names within a minute
horus cluster zone rm prod edge.internal.example     # deletes prod's records in edge; manual records stay
horus cluster ls                                     # CLUSTER  ZONES  CREATED  LAST SEEN
```

- A zone belongs to at most one cluster (`409` otherwise). To move a zone
  from one cluster to another, remove it from the first (its records there
  are deleted), then add it to the second. The second operator registers
  its Ingresses within a minute.
- A cluster keeps at least one zone. Removing the last one is refused
  (`422`); delete the cluster instead.
- Between `cluster zone rm` and the operator's next refresh, a reconcile may
  briefly get a `422`. The operator then refreshes its zones on the spot.

### Removing a cluster or zone

```sh
horus cluster rm prod                  # also deletes every record owned by prod/...
horus zone rm prod.internal.example    # refused while it belongs to a cluster; deletes all records of the zone
```

Uninstall the operator from the cluster first. Otherwise its garbage
collection and reconciles start failing with `401`.

### Monitoring

`GET /metrics` on every API listener (Prometheus text format, no auth):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `horus_dns_queries_total` | counter | `result` | DNS queries by outcome: `authoritative` (answered by horus, including chased CNAMEs), `nodata` (a name horus holds, other type), `forwarded`, `cache_hit` (forwarded, answered from the cache; includes names in our zones that horus does not hold), `servfail`, `refused`, `formerr`, `notimp`, `badvers` |
| `horus_upstream_errors_total` | counter | `upstream` | Failed upstream exchanges (timeouts, network errors, SERVFAIL/REFUSED answers) |
| `horus_cache_entries` | gauge | | Entries in the forwarding cache |
| `horus_zones` | gauge | | Zones in the served snapshot |
| `horus_records` | gauge | | Records in the served snapshot |

Useful alerts: `rate(horus_dns_queries_total{result="servfail"}[5m])` rising,
`horus_upstream_errors_total` increasing for every upstream, the target
down, and `horus cluster ls` showing a stale *LAST SEEN* for a cluster whose
operator should be running.

The operator exposes the standard controller-runtime metrics on `:8080`
(reconcile counts, errors, queue depth).

### Logs

JSON lines on stderr, collected by journald:

```sh
journalctl -u horus -o cat | jq .
```

- Every API request is logged (`method`, `path`, `status`, `dur`), except
  `/healthz` and `/metrics`.
- Upstream failures: `warn` when a forward ends in SERVFAIL; per-upstream
  failures at `debug`.
- `HORUS_QUERY_LOG=true` logs every DNS query.

### Upgrading

```sh
git pull && make build && make install      # or copy a release binary to /usr/local/bin/horus
systemctl restart horus
```

Schema migrations run automatically at start (tracked with
`PRAGMA user_version`). Take a backup first: there is no automatic downgrade,
and an older binary cannot open a migrated database. To go back, restore the
backup taken before the upgrade.

- **v0.2 → v0.3**: names inside a zone that horus does not hold are now
  forwarded upstream instead of answered with NXDOMAIN (no schema change).
  Zones no longer hide public names of the same domain, so broad zones such
  as `example.com` are safe. Internal names that do not exist now get the
  upstream's NXDOMAIN (TTL capped at 30s), and they SERVFAIL if no upstream
  answers.
- **v0.1 → v0.2**: clusters can own several zones (schema version 2).
  Existing clusters keep their zone and token. Upgrade the server first, then
  the operators (see [operator compatibility](#operator-flags)).

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `dns listen :53: ... address already in use (is systemd-resolved's stub listener on :53? ...)` | See the [port 53 note](#2-install-the-server-systemd). |
| `config: admin token missing or shorter than 32 characters` | `/etc/horus/admin.token` missing or too short. Create it with `horus token`. Outside systemd, set `HORUS_ADMIN_TOKEN_FILE`. |
| `api tls: ...` at start | The certificate or key is unreadable or invalid, or they do not match. |
| CLI: `dial unix /run/horus/api.sock: ... permission denied` | Run as root. |
| CLI: `connect: no such file or directory` | horus is not running, or `HORUS_API_SOCKET` differs from `--addr`. |
| Name resolves to the old address for a while | Positive TTL (record TTL or zone default). Lower the TTL with the `ttl` annotation. |
| New name gives NXDOMAIN for up to 30s | The client cached the earlier NXDOMAIN. This is expected: negative answers for names in our zones are capped at 30s. |
| A public name in one of our zones gets NODATA from horus | horus holds that name (for some other type). A held name is answered by horus alone; add the missing record to horus, or remove horus's record for that name. |
| Names in our zones SERVFAIL | horus does not hold them, so they are forwarded, and every upstream failed. Names horus holds keep working without upstreams. |
| Ingress not registered | `kubectl describe ingress`: look for events. No events at all → missing `register: "true"` annotation, or the operator is not running. `waiting for a load balancer address` in operator logs → the ingress controller has not published `status.loadBalancer`. Use the `target` annotation if it never will. |
| `HostnameConflict` | `horus owners` shows who holds the name. Remove that record or Ingress. The operator picks the name up within 5 minutes. |
| `HostSkipped: ... outside the cluster's zones` | The host's zone is not assigned to this cluster: `horus cluster zone add <cluster> <zone>` (create the zone first if needed). It is picked up within a minute. |
| `zone ... belongs to cluster ...` | A zone has at most one cluster. Remove it from the other cluster first. |
| `cluster ... has no zones` at operator start | Give the cluster a zone: `horus cluster zone add`. |
| Operator `CrashLoopBackOff`, `whoami: ...` | Wrong `HORUS_URL`, TLS trust (`--ca-file`), network/firewall to port 8053, or wrong/rotated token. |
| `the token is an admin token` | The operator needs a cluster token (`horus cluster add`). |
| Forwarded names SERVFAIL | Every upstream failed. Check `horus_upstream_errors_total` and reachability of `HORUS_UPSTREAMS`. |
| `horus backup` fails with "output file already exists" | Two backups in the same second. Retry. |

## Development

```sh
make vet test          # root module: go vet + go test -race
make operator-test     # operator module
make e2e               # kind end-to-end test (see below)
make tidy              # go mod tidy in both modules
```

### Local smoke test (no root)

```sh
make build
bin/horus token > /tmp/horus.token
HORUS_DNS_LISTEN=127.0.0.1:15353 HORUS_API_SOCKET=$PWD/api.sock \
HORUS_DB=$PWD/dev.db HORUS_ADMIN_TOKEN_FILE=/tmp/horus.token bin/horus serve &

export HORUS_ADDR=unix://$PWD/api.sock HORUS_TOKEN_FILE=/tmp/horus.token
bin/horus zone add dev.example
bin/horus record add a.dev.example A 10.0.0.1
dig @127.0.0.1 -p 15353 a.dev.example
```

On macOS, port 5353 is taken by mDNSResponder, and unix socket paths must be
shorter than 104 bytes (keep the socket in the repository or a short path).

### End-to-end test

`make e2e` ([`test/e2e/kind.sh`](test/e2e/kind.sh)):

1. builds `horus` and the operator image;
2. creates a kind cluster `horus-e2e` with **its own kubeconfig** (your
   current kubectl context is untouched);
3. runs horus on the host with a TLS API (self-signed certificate passed to
   the operator via `--ca-file`);
4. deploys the operator from `operator/config/operator.yaml`;
5. creates Ingresses and fakes their load balancer status (no ingress
   controller needed), then checks with `dig`:
   LB IP → A record, address change, LB hostname → CNAME, TTL annotation,
   first-wins conflict with a manual record (and the rest still registers),
   opt-out, Ingress deletion, and garbage collection of an Ingress deleted
   while the operator was scaled to zero;
6. deletes the cluster and stops horus.

`KEEP=1 make e2e` leaves everything running and prints how to reach it.
Variables: `CLUSTER`, `HORUS_E2E_HOST` (how pods reach the host; defaults to
`host.docker.internal` on macOS, the kind network gateway on Linux),
`DNS_PORT` (15300), `API_PORT` (18300), `TIMEOUT` (90s per check).

### Conventions

The rules for contributors (package boundaries, where logic lives, error
mapping, logging) are in [CLAUDE.md](CLAUDE.md) and
[docs/DESIGN.md](docs/DESIGN.md). In short:

- `pkg/*` is shared with the operator and stays stdlib-only.
- `internal/zone` holds the resolution rules and imports no DNS library.
- The DNS hot path never touches SQLite or takes a lock.
- All writes go through `httpapi.Server.write`/`writeIf`.
- First-wins logic lives in `internal/store/plan.go` (pure, unit-tested).
- `go test -race` must pass. Tests live next to the code.

## Repository layout

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
  cmd/                  operator main
  internal/controller/  Ingress reconciler + garbage collector
  config/               deployment manifest, example Ingress
deploy/systemd/         units, env example, TLS drop-in
test/e2e/               kind end-to-end test
docs/                   DESIGN.md, API.md
```

## Versioning and releases

Releases are git tags `vMAJOR.MINOR.PATCH`
([semantic versioning](https://semver.org/)); `vX.Y.Z-rc.N` and similar are
published as pre-releases. Before 1.0, minor versions may change the API or
behavior. Each [release](https://github.com/Warewave-Technology/horus/releases)
has generated notes, static Linux binaries (`amd64`, `arm64`) of `horus` and
`horus-operator` with a `SHA256SUMS` file, and the multi-arch operator image
`ghcr.io/warewave-technology/horus-operator:<tag>`. `:latest` points to the
newest non-pre-release.

`v0.1.0` predates the pipeline: it has binaries but no image. Use `v0.1.1` or
later for the operator.

### CI

GitHub Actions, on the organization's self-hosted runners
([`.github/workflows`](.github/workflows)):

| Workflow | Trigger | What it does |
|---|---|---|
| `ci` | push to `main`, pull requests, manual, called by `release` | `gofmt`, `go.mod` tidy check, `make vet test operator-test build operator` inside a `golang:1.26` container |
| `release` | tag `v*.*.*` | runs `ci`, then in parallel builds the binaries (and checks that `horus version` prints the tag) and pushes the image to GHCR with `GITHUB_TOKEN`; finally creates the GitHub release with the binaries attached |

Requirements for the runners:

- Linux with Docker. Jobs run in job containers, and the image is built with
  buildx. Go and gcc come from the container. The Dockerfile cross-compiles,
  so no QEMU is needed for the second architecture.
- The organization runner group must allow **public repositories**
  (Organization settings → Actions → Runner groups). Otherwise jobs wait in
  the queue forever.
- Labels: jobs use `["self-hosted","linux"]`. Override this with the
  repository variable `RUNNER_LABELS`, a JSON array, for example
  `gh variable set RUNNER_LABELS --body '["self-hosted","linux","x64"]'`.
- Pull requests from forks are skipped on purpose, because their code would
  run on the self-hosted runners. Also keep "Require approval for all
  outside collaborators" enabled in the repository's Actions settings.

`make e2e` is not part of CI. Run it before releases that touch the operator
or DNS behavior.

### Cutting a release

```sh
# 1. main is green in CI; update the image tag in operator/config/operator.yaml
sed -i '' 's#horus-operator:v[0-9.]*#horus-operator:v0.2.0#' operator/config/operator.yaml   # GNU sed: sed -i
git commit -am "Release v0.2.0"
git push

# 2. tag and push; the release workflow does the rest
git tag -a v0.2.0 -m "horus v0.2.0"
git push origin v0.2.0
gh run watch "$(gh run list --workflow release --limit 1 --json databaseId --jq '.[0].databaseId')"
```

The image is linked to this repository through its OCI `source` label and
inherits its public visibility, so clusters pull it without credentials. If
the organization's package settings ever make it private, change it on the
package page (Package settings → Change visibility) or give the clusters an
image pull secret. Fixing a broken release: delete the
release and the tag (`gh release delete vX.Y.Z --cleanup-tag`), fix the
problem, and tag again. Never move a tag that has already been pulled.

## License

[MIT](LICENSE) © 2026 Warewave Technology
