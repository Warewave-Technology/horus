-- horus schema, version 1.
-- Applied by store.Open inside a transaction when PRAGMA user_version < 1.

CREATE TABLE zones (
  id          INTEGER PRIMARY KEY,
  name        TEXT    NOT NULL UNIQUE,       -- lowercase FQDN, trailing dot
  default_ttl INTEGER NOT NULL DEFAULT 300,
  serial      INTEGER NOT NULL DEFAULT 1,    -- bumped on every write in the zone
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);

CREATE TABLE clusters (
  id          TEXT    PRIMARY KEY,           -- "prod", "staging"
  zone_id     INTEGER NOT NULL UNIQUE REFERENCES zones(id),
  token_hash  BLOB    NOT NULL UNIQUE,       -- sha256(token)
  created_at  INTEGER NOT NULL,
  last_seen   INTEGER
);

CREATE TABLE records (
  id          INTEGER PRIMARY KEY,
  zone_id     INTEGER NOT NULL REFERENCES zones(id) ON DELETE CASCADE,
  name        TEXT    NOT NULL,              -- FQDN
  type        TEXT    NOT NULL CHECK (type IN ('A','AAAA','CNAME')),
  data        TEXT    NOT NULL,              -- IP or FQDN
  ttl         INTEGER NOT NULL DEFAULT 0,    -- 0 = zone default
  owner       TEXT    NOT NULL,              -- "manual" | "<cluster>/<ns>/<ingress>"
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  UNIQUE (name, type, data)
);
CREATE INDEX idx_records_owner ON records(owner);
CREATE INDEX idx_records_name  ON records(name);

CREATE TABLE audit_log (
  id     INTEGER PRIMARY KEY,
  ts     INTEGER NOT NULL,
  actor  TEXT    NOT NULL,                   -- "admin" | "cluster:<id>"
  action TEXT    NOT NULL,                   -- "zone.create", "owner.replace", ...
  target TEXT,                               -- zone name, owner, record id, ...
  detail TEXT                                -- JSON
);
CREATE INDEX idx_audit_ts ON audit_log(ts);
