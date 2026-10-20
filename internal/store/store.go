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
	"strings"
	"time"

	"github.com/warewave/horus/pkg/api"
)

//go:embed schema.sql
var schemaV1 string

const schemaVersion = 1

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
		if v < 1 {
			if _, err := tx.ExecContext(ctx, schemaV1); err != nil {
				return err
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
		err = tx.QueryRowContext(ctx, `SELECT id FROM clusters WHERE zone_id = ?`, z.ID).Scan(&cluster)
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

// ReplaceOwner makes the records of owner equal to desired, within zone.
// Names held by other owners are skipped and reported (first-wins); everything
// else is applied atomically.
func (s *Store) ReplaceOwner(ctx context.Context, actor, zone, owner string, desired []api.Record) (api.OwnerRecordsResponse, error) {
	var resp api.OwnerRecordsResponse
	ts := s.ts()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		z, err := zoneByName(ctx, tx, api.NormalizeName(zone))
		if err != nil {
			return err
		}
		names := map[string]bool{}
		for i := range desired {
			if err := api.ValidateRecord(&desired[i], z.Name); err != nil {
				return err
			}
			desired[i].Zone, desired[i].Owner = z.Name, owner
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
			if _, err := tx.ExecContext(ctx, `UPDATE records SET ttl = ?, updated_at = ? WHERE id = ?`, r.TTL, ts, r.ID); err != nil {
				return err
			}
			touched[z.ID] = true
		}
		for _, r := range p.insert {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO records (zone_id, name, type, data, ttl, owner, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				z.ID, r.Name, string(r.Type), r.Data, r.TTL, owner, ts, ts); err != nil {
				return err
			}
			touched[z.ID] = true
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

// ListClusters returns all clusters (without tokens).
func (s *Store) ListClusters(ctx context.Context) ([]api.Cluster, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, z.name, c.created_at, COALESCE(c.last_seen, 0)
		 FROM clusters c JOIN zones z ON z.id = c.zone_id ORDER BY c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Cluster
	for rows.Next() {
		var c api.Cluster
		if err := rows.Scan(&c.ID, &c.Zone, &c.CreatedAt, &c.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CreateCluster binds a new cluster to a zone. tokenHash is sha256(token).
func (s *Store) CreateCluster(ctx context.Context, actor, id, zone string, tokenHash []byte) (api.Cluster, error) {
	c := api.Cluster{ID: id, Zone: api.NormalizeName(zone)}
	if err := api.ValidateClusterID(id); err != nil {
		return c, err
	}
	ts := s.ts()
	err := s.tx(ctx, func(tx *sql.Tx) error {
		z, err := zoneByName(ctx, tx, c.Zone)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO clusters (id, zone_id, token_hash, created_at) VALUES (?, ?, ?, ?)`,
			id, z.ID, tokenHash, ts)
		if isUnique(err) {
			return fmt.Errorf("cluster %q or a cluster for zone %q: %w", id, c.Zone, ErrExists)
		}
		if err != nil {
			return err
		}
		c.CreatedAt = ts
		return audit(ctx, tx, ts, actor, "cluster.create", id, map[string]string{"zone": c.Zone})
	})
	return c, err
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

// DeleteCluster removes a cluster and every record it owns.
func (s *Store) DeleteCluster(ctx context.Context, actor, id string) error {
	ts := s.ts()
	return s.tx(ctx, func(tx *sql.Tx) error {
		var zoneID int64
		err := tx.QueryRowContext(ctx, `SELECT zone_id FROM clusters WHERE id = ?`, id).Scan(&zoneID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("cluster %q: %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM records WHERE substr(owner, 1, ?) = ?`, len(id)+1, id+"/")
		if err != nil {
			return err
		}
		removed, _ := res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `DELETE FROM clusters WHERE id = ?`, id); err != nil {
			return err
		}
		if err := bumpSerial(ctx, tx, zoneID, ts); err != nil {
			return err
		}
		return audit(ctx, tx, ts, actor, "cluster.delete", id, map[string]int64{"records_removed": removed})
	})
}

// ClusterByTokenHash authenticates a cluster token and records last_seen.
func (s *Store) ClusterByTokenHash(ctx context.Context, tokenHash []byte) (api.Cluster, error) {
	var c api.Cluster
	err := s.db.QueryRowContext(ctx,
		`SELECT c.id, z.name FROM clusters c JOIN zones z ON z.id = c.zone_id WHERE c.token_hash = ?`,
		tokenHash).Scan(&c.ID, &c.Zone)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	// Best effort; a failed last_seen update must not fail authentication.
	_, _ = s.db.ExecContext(ctx, `UPDATE clusters SET last_seen = ? WHERE id = ?`, s.ts(), c.ID)
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
