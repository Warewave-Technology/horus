# horus REST API (v1)

All requests: `Authorization: Bearer <token>`, JSON bodies (unknown fields are
rejected), max body 1 MiB. Names are normalized server-side to lowercase FQDN
with a trailing dot; both `app.example` and `App.Example.` are accepted.

Two kinds of token:

- **admin** — from `/etc/horus/admin.token`; used by the CLI.
- **cluster** — `hrs_...`, created by `POST /v1/clusters`, stored as sha256.
  Bound to one cluster id and one zone.

Errors: `{"error": "...", "conflicts": [{"name": "...", "held_by": "..."}]}`

| Status | When |
|---|---|
| 401 | missing/unknown token |
| 403 | wrong token kind for the endpoint |
| 404 | zone / record / cluster not found |
| 409 | already exists, or first-wins conflict (`conflicts` set) |
| 422 | validation failed (bad name, type, IP, out of zone, CNAME rules, unknown JSON field) |
| 500 | internal; "write committed but DNS snapshot reload failed" means check logs |

Every successful write is live in DNS when the response is sent.

## Public

| Method | Path | |
|---|---|---|
| GET | `/healthz` | `ok` |
| GET | `/metrics` | Prometheus text format |

## Any token

### `GET /v1/whoami`
```json
{"admin": false, "cluster": "prod", "zone": "prod.internal.example."}
```

## Admin token

### Zones
| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/v1/zones` | | `[Zone]` |
| POST | `/v1/zones` | `{"name": "prod.internal.example", "default_ttl": 300}` | 201 `Zone` |
| DELETE | `/v1/zones/{zone}` | | 204; 409 while a cluster is bound to it |

`Zone`: `{"id", "name", "default_ttl", "serial", "created_at", "updated_at"}`.
`default_ttl` 0/omitted → 300. Overlapping zones are rejected (422).

### Records
| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/v1/zones/{zone}/records` | | `[Record]` |
| POST | `/v1/zones/{zone}/records` | `{"name", "type", "data", "ttl"}` | 201 `Record` (owner `manual`) |
| DELETE | `/v1/records/{id}` | | 204; 409 if the record is operator-owned |

`Record`: `{"id", "zone", "name", "type": "A|AAAA|CNAME", "data", "ttl", "owner", "created_at", "updated_at"}`.
`ttl` 0/omitted → zone default.

### Clusters
| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/v1/clusters` | | `[Cluster]` (no tokens) |
| POST | `/v1/clusters` | `{"id": "prod", "zone": "prod.internal.example"}` | 201 `Cluster` **with `token` (only time it is shown)** |
| POST | `/v1/clusters/{id}/rotate-token` | | `{"id", "token"}`; old token stops working immediately |
| DELETE | `/v1/clusters/{id}` | | 204; also deletes every record owned by the cluster |

`id`: `[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`. One cluster per zone.

### Backup
| Method | Path | Response |
|---|---|---|
| POST | `/v1/backup` | 201 `{"path": "/var/lib/horus/backups/horus-20261020T120000Z.db"}` |

## Cluster token

Owner = `<cluster>/<namespace>/<name>`; the cluster part comes from the token.
`namespace` must be a DNS-1123 label, `name` a DNS-1123 subdomain.

### `GET /v1/owners`
```json
{"owners": ["default/web", "shop/api"]}
```

### `PUT /v1/owners/{namespace}/{name}/records`
The complete desired set for this owner. Records not listed are removed.
Records must not set `id`, `owner`, or a `zone` other than the cluster's.
```json
{"records": [
  {"name": "web.prod.internal.example.", "type": "A", "data": "10.20.0.15", "ttl": 60},
  {"name": "web.prod.internal.example.", "type": "AAAA", "data": "2001:db8::15"}
]}
```
Response (always 200 when the request is valid, even with conflicts):
```json
{"added": 1, "updated": 0, "removed": 0,
 "conflicts": [{"name": "db.prod.internal.example.", "held_by": "manual"}]}
```
Conflicting names are skipped; all other names are applied atomically.
Sending the same set again returns all zeros.

### `DELETE /v1/owners/{namespace}/{name}`
204. Idempotent.
