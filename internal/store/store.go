// Package store persists zones, records, clusters and the audit log in SQLite.
//
// The store is the only writer to the database. It uses database/sql only; the
// driver (modernc.org/sqlite, registered as "sqlite") is imported by the binary.
// The connection pool is limited to a single connection: writes are serialized,
// and the DNS hot path never reads from here (it reads the in-memory snapshot).
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Warewave-Technology/horus/pkg/api"
)

//go:embed schema.sql
var schemaV1 string

//go:embed migrate_v2.sql
var migrateV2 string

// migrations[i] takes the schema from version i to i+1. Never edit an
// applied migration; append a new one.
var migrations = []string{schemaV1, migrateV2}

var schemaVersion = len(migrations)

var (
	// ErrNotFound is returned when the addressed object does not exist.
	ErrNotFound = errors.New("not found")
	// ErrExists is returned when creating something that already exists.
	ErrExists = errors.New("already exists")
)

// ConflictError is returned when a write would take a name another owner holds.
type ConflictError struct {
	Conflicts []api.Conflict
}

func (e *ConflictError) Error() string {
	parts := make([]string, 0, len(e.Conflicts))
	for _, c := range e.Conflicts {
		parts = append(parts, fmt.Sprintf("%s held by %s", c.Name, c.HeldBy))
	}
	return "conflict: " + strings.Join(parts, ", ")
}

// Store is the SQLite-backed store.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// DSN builds a modernc.org/sqlite DSN with the pragmas horus relies on.
func DSN(path string) string {
	return "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)"
}

// Open opens (and migrates) the database. driverName is "sqlite" for modernc.
func Open(ctx context.Context, driverName, dsn string) (*Store, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v >= schemaVersion {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		for i := v; i < schemaVersion; i++ {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return fmt.Errorf("schema v%d: %w", i+1, err)
			}
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
		return err
	})
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) ts() int64 { return s.now().Unix() }

func audit(ctx context.Context, tx *sql.Tx, ts int64, actor, action, target string, detail any) error {
	var d []byte
	if detail != nil {
		var err error
		if d, err = json.Marshal(detail); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, action, target, detail) VALUES (?, ?, ?, ?, ?)`,
		ts, actor, action, target, string(d))
	return err
}

func bumpSerial(ctx context.Context, tx *sql.Tx, zoneID, ts int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE zones SET serial = serial + 1, updated_at = ? WHERE id = ?`, ts, zoneID)
	return err
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ---------------------------------------------------------------- zones

const zoneCols = `id, name, default_ttl, serial, created_at, updated_at`

func scanZone(row interface{ Scan(...any) error }) (api.Zone, error) {
	var z api.Zone
	err := row.Scan(&z.ID, &z.Name, &z.DefaultTTL, &z.Serial, &z.CreatedAt, &z.UpdatedAt)
	return z, err
}

// ListZones returns all zones ordered by name.
func (s *Store) ListZones(ctx context.Context) ([]api.Zone, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+zoneCols+` FROM zones ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Zone
	for rows.Next() {
		z, err := scanZone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, z)
	}
	return out, rows.Err()
}

func zoneByName(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, name string) (api.Zone, error) {
	z, err := scanZone(q.QueryRowContext(ctx, `SELECT `+zoneCols+` FROM zones WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return z, fmt.Errorf("zone %q: %w", name, ErrNotFound)
	}
	return z, err
}

// GetZone returns a zone by (normalized) name.
func (s *Store) GetZone(ctx context.Context, name string) (api.Zone, error) {
	return zoneByName(ctx, s.db, api.NormalizeName(name))
}

// CreateZone creates a zone. defaultTTL 0 means 300.
func (s *Store) CreateZone(ctx context.Context, actor string, z api.Zone) (api.Zone, error) {
	if err := api.ValidateZone(&z); err != nil {
		return z, err
	}
	if z.DefaultTTL == 0 {
		z.DefaultTTL = 300
	}
	ts := s.ts()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// Refuse overlapping zones: one name must map to exactly one zone so
		// first-wins ownership stays unambiguous.
		existing, err := listZoneNames(ctx, tx)
		if err != nil {
			return err
		}
		for _, ex := range existing {
			if ex == z.Name {
				return fmt.Errorf("zone %q: %w", z.Name, ErrExists)
			}
			if api.InZone(z.Name, ex) || api.InZone(ex, z.Name) {
				return fmt.Errorf("%w: zone %q overlaps existing zone %q", api.ErrInvalid, z.Name, ex)
			}
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO zones (name, default_ttl, serial, created_at, updated_at) VALUES (?, ?, 1, ?, ?)`,
			z.Name, z.DefaultTTL, ts, ts)
		if err != nil {
			return err
		}
		z.ID, _ = res.LastInsertId()
		z.Serial, z.CreatedAt, z.UpdatedAt = 1, ts, ts
		return audit(ctx, tx, ts, actor, "zone.create", z.Name, z)
	})
	return z, err
}

func listZoneNames(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM zones`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DeleteZone deletes a zone and all its records. It refuses while a cluster
// is still bound to the zone.
func (s *Store) DeleteZone(ctx context.Context, actor, name string) error {
	name = api.NormalizeName(name)
	ts := s.ts()
	return s.tx(ctx, func(tx *sql.Tx) error {
		z, err := zoneByName(ctx, tx, name)
		if err != nil {
			return err
		}
		var cluster string
		err = tx.QueryRowContext(ctx, `SELECT cluster_id FROM cluster_zones WHERE zone_id = ?`, z.ID).Scan(&cluster)
		if err == nil {
			return &ConflictError{Conflicts: []api.Conflict{{Name: name, HeldBy: "cluster:" + cluster}}}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM zones WHERE id = ?`, z.ID); err != nil {
			return err
		}
		return audit(ctx, tx, ts, actor, "zone.delete", name, nil)
	})
}

// ---------------------------------------------------------------- records

const recordCols = `r.id, z.name, r.name, r.type, r.data, r.ttl, r.owner, r.created_at, r.updated_at`
const recordFrom = ` FROM records r JOIN zones z ON z.id = r.zone_id`

func scanRecords(rows *sql.Rows) ([]api.Record, error) {
	defer rows.Close()
	var out []api.Record
	for rows.Next() {
		var r api.Record
		var typ string
		if err := rows.Scan(&r.ID, &r.Zone, &r.Name, &typ, &r.Data, &r.TTL, &r.Owner, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Type = api.RecordType(typ)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRecords returns the records of a zone ordered by name, type, data.
func (s *Store) ListRecords(ctx context.Context, zone string) ([]api.Record, error) {
	z, err := s.GetZone(ctx, zone)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+recordCols+recordFrom+` WHERE r.zone_id = ? ORDER BY r.name, r.type, r.data`, z.ID)
	if err != nil {
		return nil, err
	}
	return scanRecords(rows)
}

func recordsByNames(ctx context.Context, tx *sql.Tx, names []string) (map[string][]api.Record, error) {
	out := map[string][]api.Record{}
	if len(names) == 0 {
		return out, nil
	}
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	q := `SELECT ` + recordCols + recordFrom + ` WHERE r.name IN (?` + strings.Repeat(",?", len(names)-1) + `)`
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	recs, err := scanRecords(rows)
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		out[r.Name] = append(out[r.Name], r)
	}
	return out, nil
}

// AddRecord creates a manual record. The name must not be held by another
// owner, and the CNAME rules must still hold for the name afterwards.
func (s *Store) AddRecord(ctx context.Context, actor string, r api.Record) (api.Record, error) {
	ts := s.ts()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		z, err := zoneByName(ctx, tx, api.NormalizeName(r.Zone))
		if err != nil {
			return err
		}
		if err := api.ValidateRecord(&r, z.Name); err != nil {
			return err
		}
		r.Zone, r.Owner = z.Name, api.OwnerManual
		existing, err := recordsByNames(ctx, tx, []string{r.Name})
		if err != nil {
			return err
		}
		set := []api.Record{r}
		for _, ex := range existing[r.Name] {
			if ex.Owner != api.OwnerManual {
				return &ConflictError{Conflicts: []api.Conflict{{Name: r.Name, HeldBy: ex.Owner}}}
			}
			if ex.Key() == r.Key() {
				return fmt.Errorf("record %s: %w", r.Key(), ErrExists)
			}
			set = append(set, ex)
		}
		if err := api.ValidateRRSet(set); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO records (zone_id, name, type, data, ttl, owner, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			z.ID, r.Name, string(r.Type), r.Data, r.TTL, r.Owner, ts, ts)
		if err != nil {
			if isUnique(err) {
				return fmt.Errorf("record %s: %w", r.Key(), ErrExists)
			}
			return err
		}
		r.ID, _ = res.LastInsertId()
		r.CreatedAt, r.UpdatedAt = ts, ts
		if err := bumpSerial(ctx, tx, z.ID, ts); err != nil {
			return err
		}
		return audit(ctx, tx, ts, actor, "record.create", r.Name, r)
	})
	return r, err
}

// DeleteRecord deletes a record by id. Only manual records can be deleted this
// way; operator-owned records are removed by their owner (or by DeleteOwner).
func (s *Store) DeleteRecord(ctx context.Context, actor string, id int64) error {
	ts := s.ts()
	return s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+recordCols+recordFrom+` WHERE r.id = ?`, id)
		if err != nil {
			return err
		}
		recs, err := scanRecords(rows)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			return fmt.Errorf("record %d: %w", id, ErrNotFound)
		}
		r := recs[0]
		if r.Owner != api.OwnerManual {
			return &ConflictError{Conflicts: []api.Conflict{{Name: r.Name, HeldBy: r.Owner}}}
		}
		var zoneID int64
		if err := tx.QueryRowContext(ctx, `SELECT zone_id FROM records WHERE id = ?`, id).Scan(&zoneID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM records WHERE id = ?`, id); err != nil {
			return err
		}
		if err := bumpSerial(ctx, tx, zoneID, ts); err != nil {
			return err
		}
		return audit(ctx, tx, ts, actor, "record.delete", r.Name, r)
	})
}

// ---------------------------------------------------------------- owners

// ReplaceOwner makes the records of owner equal to desired. Every record must
// lie in one of the zones of cluster, read inside the transaction so that a
// concurrent `cluster zone rm` cannot be undone. Names held by other owners
// are skipped and reported (first-wins); everything else is applied
// atomically.
func (s *Store) ReplaceOwner(ctx context.Context, actor, cluster, owner string, desired []api.Record) (api.OwnerRecordsResponse, error) {
	var resp api.OwnerRecordsResponse
	ts := s.ts()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if !strings.HasPrefix(owner, cluster+"/") {
			return fmt.Errorf("owner %q is not under cluster %q", owner, cluster)
		}
		allowed, err := clusterZones(ctx, tx, cluster)
		if err != nil {
			return err
		}
		if len(allowed) == 0 {
			return fmt.Errorf("cluster %q: %w", cluster, ErrNotFound)
		}
		zoneIDs := map[string]int64{}
		for _, name := range allowed {
			z, err := zoneByName(ctx, tx, name)
			if err != nil {
				return err
			}
			zoneIDs[z.Name] = z.ID
		}
		names := map[string]bool{}
		for i := range desired {
			zn := api.ZoneFor(api.NormalizeName(desired[i].Name), allowed)
			if zn == "" {
				return fmt.Errorf("%w: name %q is outside the cluster's zones (%s)",
					api.ErrInvalid, desired[i].Name, strings.Join(allowed, ", "))
			}
			if err := api.ValidateRecord(&desired[i], zn); err != nil {
				return err
			}
			desired[i].Zone, desired[i].Owner = zn, owner
			names[desired[i].Name] = true
		}
		if err := api.ValidateRRSet(desired); err != nil {
			return err
		}

		rows, err := tx.QueryContext(ctx, `SELECT `+recordCols+recordFrom+` WHERE r.owner = ?`, owner)
		if err != nil {
			return err
		}
		owned, err := scanRecords(rows)
		if err != nil {
			return err
		}
		nameList := make([]string, 0, len(names))
		for n := range names {
			nameList = append(nameList, n)
		}
		byName, err := recordsByNames(ctx, tx, nameList)
		if err != nil {
			return err
		}

		p := planReplace(owner, desired, owned, byName)
		resp.Conflicts = p.conflicts
		touched := map[int64]bool{} // zone ids whose serial must be bumped

		for _, id := range p.delete {
			var zid int64
			if err := tx.QueryRowContext(ctx, `DELETE FROM records WHERE id = ? RETURNING zone_id`, id).Scan(&zid); err != nil {
				return err
			}
			touched[zid] = true
		}
		for _, r := range p.update {
			var zid int64
			if err := tx.QueryRowContext(ctx, `UPDATE records SET ttl = ?, updated_at = ? WHERE id = ? RETURNING zone_id`,
				r.TTL, ts, r.ID).Scan(&zid); err != nil {
				return err
			}
			touched[zid] = true
		}
		for _, r := range p.insert {
			zid := zoneIDs[r.Zone]
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO records (zone_id, name, type, data, ttl, owner, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				zid, r.Name, string(r.Type), r.Data, r.TTL, owner, ts, ts); err != nil {
				return err
			}
			touched[zid] = true
		}
		resp.Added, resp.Updated, resp.Removed = len(p.insert), len(p.update), len(p.delete)
		for zid := range touched {
			if err := bumpSerial(ctx, tx, zid, ts); err != nil {
				return err
			}
		}
		if resp.Changed() || len(resp.Conflicts) > 0 {
			return audit(ctx, tx, ts, actor, "owner.replace", owner, map[string]any{
				"insert": p.insert, "update": p.update, "delete": p.delete, "conflicts": p.conflicts,
			})
		}
		return nil
	})
	return resp, err
}

// DeleteOwner removes every record of owner and returns how many were removed.
func (s *Store) DeleteOwner(ctx context.Context, actor, owner string) (int, error) {
	var n int
	ts := s.ts()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `DELETE FROM records WHERE owner = ? RETURNING zone_id`, owner)
		if err != nil {
			return err
		}
		zones := map[int64]bool{}
		for rows.Next() {
			var zid int64
			if err := rows.Scan(&zid); err != nil {
				rows.Close()
				return err
			}
			zones[zid] = true
			n++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for zid := range zones {
			if err := bumpSerial(ctx, tx, zid, ts); err != nil {
				return err
			}
		}
		if n == 0 {
			return nil
		}
		return audit(ctx, tx, ts, actor, "owner.delete", owner, map[string]int{"removed": n})
	})
	return n, err
}

// ListOwners returns the distinct owners starting with prefix.
func (s *Store) ListOwners(ctx context.Context, prefix string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT owner FROM records WHERE substr(owner, 1, ?) = ? ORDER BY owner`,
		len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- clusters

// ListClusters returns all clusters (without tokens) with their zones.
func (s *Store) ListClusters(ctx context.Context) ([]api.Cluster, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at, COALESCE(last_seen, 0) FROM clusters ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var out []api.Cluster
	idx := map[string]int{}
	for rows.Next() {
		var c api.Cluster
		if err := rows.Scan(&c.ID, &c.CreatedAt, &c.LastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		idx[c.ID] = len(out)
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx,
		`SELECT cz.cluster_id, z.name FROM cluster_zones cz JOIN zones z ON z.id = cz.zone_id ORDER BY z.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, zone string
		if err := rows.Scan(&id, &zone); err != nil {
			return nil, err
		}
		if i, ok := idx[id]; ok {
			out[i].Zones = append(out[i].Zones, zone)
		}
	}
	return out, rows.Err()
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// clusterZones returns the zones of a cluster, sorted.
func clusterZones(ctx context.Context, q queryer, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT z.name FROM cluster_zones cz JOIN zones z ON z.id = cz.zone_id
		 WHERE cz.cluster_id = ? ORDER BY z.name`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func clusterExists(ctx context.Context, tx *sql.Tx, id string) error {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM clusters WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("cluster %q: %w", id, ErrNotFound)
	}
	return err
}

// assignZone gives zone z to cluster id. A zone belongs to at most one cluster.
func assignZone(ctx context.Context, tx *sql.Tx, id string, z api.Zone) error {
	var holder string
	err := tx.QueryRowContext(ctx, `SELECT cluster_id FROM cluster_zones WHERE zone_id = ?`, z.ID).Scan(&holder)
	switch {
	case err == nil && holder == id:
		return fmt.Errorf("cluster %q already has zone %q: %w", id, z.Name, ErrExists)
	case err == nil:
		return fmt.Errorf("zone %q belongs to cluster %q: %w", z.Name, holder, ErrExists)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO cluster_zones (cluster_id, zone_id) VALUES (?, ?)`, id, z.ID)
	return err
}

// CreateCluster creates a cluster that may write to zones (at least one).
// tokenHash is sha256(token).
func (s *Store) CreateCluster(ctx context.Context, actor, id string, zones []string, tokenHash []byte) (api.Cluster, error) {
	c := api.Cluster{ID: id}
	if err := api.ValidateClusterID(id); err != nil {
		return c, err
	}
	seen := map[string]bool{}
	for _, z := range zones {
		if z = api.NormalizeName(z); z != "" && !seen[z] {
			seen[z] = true
			c.Zones = append(c.Zones, z)
		}
	}
	if len(c.Zones) == 0 {
		return c, fmt.Errorf("%w: a cluster needs at least one zone", api.ErrInvalid)
	}
	sort.Strings(c.Zones)
	ts := s.ts()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO clusters (id, token_hash, created_at) VALUES (?, ?, ?)`, id, tokenHash, ts)
		if isUnique(err) {
			return fmt.Errorf("cluster %q: %w", id, ErrExists)
		}
		if err != nil {
			return err
		}
		for _, name := range c.Zones {
			z, err := zoneByName(ctx, tx, name)
			if err != nil {
				return err
			}
			if err := assignZone(ctx, tx, id, z); err != nil {
				return err
			}
		}
		c.CreatedAt = ts
		return audit(ctx, tx, ts, actor, "cluster.create", id, map[string]any{"zones": c.Zones})
	})
	return c, err
}

// AddClusterZone gives one more zone to a cluster and returns the cluster.
func (s *Store) AddClusterZone(ctx context.Context, actor, id, zone string) (api.Cluster, error) {
	zone = api.NormalizeName(zone)
	ts := s.ts()
	var c api.Cluster
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := clusterExists(ctx, tx, id); err != nil {
			return err
		}
		z, err := zoneByName(ctx, tx, zone)
		if err != nil {
			return err
		}
		if err := assignZone(ctx, tx, id, z); err != nil {
			return err
		}
		if c.Zones, err = clusterZones(ctx, tx, id); err != nil {
			return err
		}
		c.ID = id
		return audit(ctx, tx, ts, actor, "cluster.zone.add", id, map[string]string{"zone": z.Name})
	})
	return c, err
}

// RemoveClusterZone takes a zone away from a cluster and deletes the records
// the cluster owns in it. It returns how many records were removed. The last
// zone of a cluster cannot be removed (delete the cluster instead).
func (s *Store) RemoveClusterZone(ctx context.Context, actor, id, zone string) (int, error) {
	zone = api.NormalizeName(zone)
	ts := s.ts()
	var removed int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := clusterExists(ctx, tx, id); err != nil {
			return err
		}
		zones, err := clusterZones(ctx, tx, id)
		if err != nil {
			return err
		}
		if !slices.Contains(zones, zone) {
			return fmt.Errorf("cluster %q has no zone %q: %w", id, zone, ErrNotFound)
		}
		if len(zones) == 1 {
			return fmt.Errorf("%w: %q is the last zone of cluster %q; delete the cluster instead", api.ErrInvalid, zone, id)
		}
		z, err := zoneByName(ctx, tx, zone)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM records WHERE zone_id = ? AND substr(owner, 1, ?) = ?`, z.ID, len(id)+1, id+"/")
		if err != nil {
			return err
		}
		removed, _ = res.RowsAffected()
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM cluster_zones WHERE cluster_id = ? AND zone_id = ?`, id, z.ID); err != nil {
			return err
		}
		if removed > 0 {
			if err := bumpSerial(ctx, tx, z.ID, ts); err != nil {
				return err
			}
		}
		return audit(ctx, tx, ts, actor, "cluster.zone.remove", id,
			map[string]any{"zone": z.Name, "records_removed": removed})
	})
	return int(removed), err
}

// RotateClusterToken replaces a cluster's token hash.
func (s *Store) RotateClusterToken(ctx context.Context, actor, id string, tokenHash []byte) error {
	ts := s.ts()
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE clusters SET token_hash = ? WHERE id = ?`, tokenHash, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("cluster %q: %w", id, ErrNotFound)
		}
		return audit(ctx, tx, ts, actor, "cluster.rotate", id, nil)
	})
}

// DeleteCluster removes a cluster, its zone assignments and every record it owns.
func (s *Store) DeleteCluster(ctx context.Context, actor, id string) error {
	ts := s.ts()
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := clusterExists(ctx, tx, id); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`DELETE FROM records WHERE substr(owner, 1, ?) = ? RETURNING zone_id`, len(id)+1, id+"/")
		if err != nil {
			return err
		}
		touched := map[int64]bool{}
		removed := 0
		for rows.Next() {
			var zid int64
			if err := rows.Scan(&zid); err != nil {
				rows.Close()
				return err
			}
			touched[zid] = true
			removed++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// cluster_zones rows go with the cluster (ON DELETE CASCADE).
		if _, err := tx.ExecContext(ctx, `DELETE FROM clusters WHERE id = ?`, id); err != nil {
			return err
		}
		for zid := range touched {
			if err := bumpSerial(ctx, tx, zid, ts); err != nil {
				return err
			}
		}
		return audit(ctx, tx, ts, actor, "cluster.delete", id, map[string]int{"records_removed": removed})
	})
}

// lastSeenInterval limits how often authentication writes clusters.last_seen:
// operators call the API on every reconcile and readiness probe, and a
// minute's precision is plenty for "is this cluster still talking to us".
const lastSeenInterval = time.Minute

// ClusterByTokenHash authenticates a cluster token and records last_seen
// (at most once per lastSeenInterval). The cluster comes with its zones.
func (s *Store) ClusterByTokenHash(ctx context.Context, tokenHash []byte) (api.Cluster, error) {
	var c api.Cluster
	err := s.db.QueryRowContext(ctx, `SELECT id FROM clusters WHERE token_hash = ?`, tokenHash).Scan(&c.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	if c.Zones, err = clusterZones(ctx, s.db, c.ID); err != nil {
		return c, err
	}
	// This bookkeeping write bypasses httpapi's write lock on purpose: it
	// changes nothing the DNS snapshot depends on.
	ts := s.ts()
	_, _ = s.db.ExecContext(ctx,
		`UPDATE clusters SET last_seen = ? WHERE id = ? AND (last_seen IS NULL OR last_seen <= ?)`,
		ts, c.ID, ts-int64(lastSeenInterval/time.Second))
	return c, nil
}

// ---------------------------------------------------------------- snapshot / backup

// LoadAll returns every zone and record, for building the in-memory snapshot.
func (s *Store) LoadAll(ctx context.Context) ([]api.Zone, []api.Record, error) {
	zones, err := s.ListZones(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+recordCols+recordFrom)
	if err != nil {
		return nil, nil, err
	}
	recs, err := scanRecords(rows)
	return zones, recs, err
}

// Backup writes a consistent copy of the database to path (VACUUM INTO).
// path must not exist yet.
func (s *Store) Backup(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}
