-- horus schema, version 2: a cluster may own several zones.
-- Applied by store.Open after schema.sql (version 1) when PRAGMA user_version < 2.
--
-- clusters.zone_id moves to cluster_zones. SQLite cannot drop a UNIQUE
-- column, so clusters is rebuilt. The old table is dropped before
-- cluster_zones exists: with foreign keys on, dropping a parent table runs an
-- implicit DELETE that would cascade into it.

CREATE TEMP TABLE cluster_zones_v1 AS SELECT id AS cluster_id, zone_id FROM clusters;

CREATE TABLE clusters_v2 (
  id          TEXT    PRIMARY KEY,           -- "prod", "staging"
  token_hash  BLOB    NOT NULL UNIQUE,       -- sha256(token)
  created_at  INTEGER NOT NULL,
  last_seen   INTEGER
);
INSERT INTO clusters_v2 (id, token_hash, created_at, last_seen)
  SELECT id, token_hash, created_at, last_seen FROM clusters;
DROP TABLE clusters;
ALTER TABLE clusters_v2 RENAME TO clusters;

-- A cluster has one or more zones; a zone belongs to at most one cluster.
CREATE TABLE cluster_zones (
  cluster_id  TEXT    NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
  zone_id     INTEGER NOT NULL UNIQUE REFERENCES zones(id),
  PRIMARY KEY (cluster_id, zone_id)
);
INSERT INTO cluster_zones (cluster_id, zone_id) SELECT cluster_id, zone_id FROM cluster_zones_v1;
DROP TABLE cluster_zones_v1;
