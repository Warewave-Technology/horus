package store

// Integration tests against a real SQLite database (modernc.org/sqlite).

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Warewave-Technology/horus/pkg/api"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), "sqlite", DSN(filepath.Join(t.TempDir(), "horus.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	z, err := s.CreateZone(ctx, "admin", api.Zone{Name: "Prod.Internal.Example"})
	if err != nil {
		t.Fatal(err)
	}
	if z.Name != "prod.internal.example." || z.DefaultTTL != 300 {
		t.Fatalf("zone: %+v", z)
	}
	if _, err := s.CreateZone(ctx, "admin", api.Zone{Name: "sub.prod.internal.example."}); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("overlapping zone must be rejected, got %v", err)
	}

	// Manual record, then a cluster trying to take the same name.
	if _, err := s.AddRecord(ctx, "admin", api.Record{Zone: z.Name, Name: "db.prod.internal.example", Type: api.TypeA, Data: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCluster(ctx, "admin", "prod", []string{z.Name}, []byte("hash-1")); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClusterByTokenHash(ctx, []byte("hash-1"))
	if err != nil || c.ID != "prod" || len(c.Zones) != 1 || c.Zones[0] != z.Name {
		t.Fatalf("auth: %+v %v", c, err)
	}

	owner := "prod/default/web"
	resp, err := s.ReplaceOwner(ctx, "cluster:prod", "prod", owner, []api.Record{
		{Name: "web.prod.internal.example.", Type: api.TypeA, Data: "10.0.1.1"},
		{Name: "db.prod.internal.example.", Type: api.TypeA, Data: "10.0.1.1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Added != 1 || len(resp.Conflicts) != 1 || resp.Conflicts[0].HeldBy != api.OwnerManual {
		t.Fatalf("replace: %+v", resp)
	}

	// Idempotent.
	resp, err = s.ReplaceOwner(ctx, "cluster:prod", "prod", owner, []api.Record{
		{Name: "web.prod.internal.example.", Type: api.TypeA, Data: "10.0.1.1"},
	})
	if err != nil || resp.Changed() {
		t.Fatalf("second replace should be a no-op: %+v %v", resp, err)
	}

	owners, err := s.ListOwners(ctx, "prod/")
	if err != nil || len(owners) != 1 || owners[0] != owner {
		t.Fatalf("owners: %v %v", owners, err)
	}

	// Manual delete of operator-owned record is refused.
	recs, _ := s.ListRecords(ctx, z.Name)
	for _, r := range recs {
		if r.Owner == owner {
			var ce *ConflictError
			if err := s.DeleteRecord(ctx, "admin", r.ID); !errors.As(err, &ce) {
				t.Fatalf("expected conflict deleting operator record, got %v", err)
			}
		}
	}

	if n, err := s.DeleteOwner(ctx, "cluster:prod", owner); err != nil || n != 1 {
		t.Fatalf("delete owner: %d %v", n, err)
	}

	// Zone deletion is blocked while the cluster exists.
	var ce *ConflictError
	if err := s.DeleteZone(ctx, "admin", z.Name); !errors.As(err, &ce) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if err := s.DeleteCluster(ctx, "admin", "prod"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteZone(ctx, "admin", z.Name); err != nil {
		t.Fatal(err)
	}
	zones, recs, err := s.LoadAll(ctx)
	if err != nil || len(zones) != 0 || len(recs) != 0 {
		t.Fatalf("expected empty db, got %d zones %d records %v", len(zones), len(recs), err)
	}
}

func TestBackup(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	if _, err := s.CreateZone(ctx, "admin", api.Zone{Name: "a.example."}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(ctx, dst); err != nil {
		t.Fatal(err)
	}
	b, err := Open(ctx, "sqlite", DSN(dst))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	zones, err := b.ListZones(ctx)
	if err != nil || len(zones) != 1 {
		t.Fatalf("backup content: %v %v", zones, err)
	}
}

func TestLastSeenIsThrottled(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }
	if _, err := s.CreateZone(ctx, "admin", api.Zone{Name: "prod.example"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCluster(ctx, "admin", "prod", []string{"prod.example"}, []byte("h")); err != nil {
		t.Fatal(err)
	}
	lastSeen := func() int64 {
		t.Helper()
		cs, err := s.ListClusters(ctx)
		if err != nil || len(cs) != 1 {
			t.Fatalf("clusters: %v %v", cs, err)
		}
		return cs[0].LastSeen
	}
	auth := func() {
		t.Helper()
		if _, err := s.ClusterByTokenHash(ctx, []byte("h")); err != nil {
			t.Fatal(err)
		}
	}

	auth()
	if got := lastSeen(); got != now.Unix() {
		t.Fatalf("first auth: last_seen=%d", got)
	}
	first := now.Unix()
	now = now.Add(lastSeenInterval - time.Second)
	auth()
	if got := lastSeen(); got != first {
		t.Fatalf("within the interval last_seen must not move: %d", got)
	}
	now = now.Add(time.Second)
	auth()
	if got := lastSeen(); got != now.Unix() {
		t.Fatalf("after the interval: last_seen=%d, want %d", got, now.Unix())
	}
}

func TestClusterWithSeveralZones(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for _, z := range []string{"a.example", "b.example", "c.example", "d.example"} {
		if _, err := s.CreateZone(ctx, "admin", api.Zone{Name: z}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateCluster(ctx, "admin", "empty", nil, []byte("h0")); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("cluster without zones: %v", err)
	}
	c, err := s.CreateCluster(ctx, "admin", "prod", []string{"B.example", "a.example.", "b.example"}, []byte("h1"))
	if err != nil || strings.Join(c.Zones, ",") != "a.example.,b.example." {
		t.Fatalf("create: %+v %v", c, err)
	}
	// A zone belongs to at most one cluster; a failed create leaves nothing behind.
	if _, err := s.CreateCluster(ctx, "admin", "stage", []string{"c.example", "a.example"}, []byte("h2")); !errors.Is(err, ErrExists) {
		t.Fatalf("zone of another cluster: %v", err)
	}
	if cs, _ := s.ListClusters(ctx); len(cs) != 1 {
		t.Fatalf("clusters after failed create: %+v", cs)
	}
	if _, err := s.CreateCluster(ctx, "admin", "stage", []string{"c.example"}, []byte("h2")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddClusterZone(ctx, "admin", "stage", "a.example"); !errors.Is(err, ErrExists) {
		t.Fatalf("adding another cluster's zone: %v", err)
	}
	if _, err := s.AddClusterZone(ctx, "admin", "prod", "a.example"); !errors.Is(err, ErrExists) {
		t.Fatalf("adding a zone twice: %v", err)
	}
	if _, err := s.AddClusterZone(ctx, "admin", "nope", "d.example"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown cluster: %v", err)
	}
	if c, err = s.AddClusterZone(ctx, "admin", "prod", "d.example."); err != nil || len(c.Zones) != 3 {
		t.Fatalf("add zone: %+v %v", c, err)
	}

	// One owner, records in two zones.
	auth, err := s.ClusterByTokenHash(ctx, []byte("h1"))
	if err != nil || len(auth.Zones) != 3 {
		t.Fatalf("auth: %+v %v", auth, err)
	}
	owner := "prod/default/web"
	before := serials(t, s)
	resp, err := s.ReplaceOwner(ctx, "cluster:prod", "prod", owner, []api.Record{
		{Name: "web.a.example", Type: api.TypeA, Data: "10.0.0.1"},
		{Name: "web.b.example", Type: api.TypeA, Data: "10.0.0.1"},
	})
	if err != nil || resp.Added != 2 {
		t.Fatalf("replace: %+v %v", resp, err)
	}
	after := serials(t, s)
	if after["a.example."] != before["a.example."]+1 || after["b.example."] != before["b.example."]+1 || after["d.example."] != before["d.example."] {
		t.Fatalf("serials: before %v after %v", before, after)
	}
	recs, _ := s.ListRecords(ctx, "b.example")
	if len(recs) != 1 || recs[0].Zone != "b.example." || recs[0].Owner != owner {
		t.Fatalf("records in b: %+v", recs)
	}
	// Names outside the cluster's zones are rejected as a whole.
	if _, err := s.ReplaceOwner(ctx, "cluster:prod", "prod", owner, []api.Record{
		{Name: "web.a.example", Type: api.TypeA, Data: "10.0.0.1"},
		{Name: "web.c.example", Type: api.TypeA, Data: "10.0.0.1"},
	}); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("out of zones: %v", err)
	}

	// The zones are read inside the write: after a zone is removed, a PUT that
	// was authorized with the old zone list cannot write there.
	if _, err := s.ReplaceOwner(ctx, "cluster:prod", "prod", "stage/default/web", nil); err == nil {
		t.Fatal("owner outside the cluster's prefix must be refused")
	}

	// Removing a zone deletes the cluster's records there, nothing else.
	if _, err := s.AddRecord(ctx, "admin", api.Record{Zone: "b.example.", Name: "manual.b.example.", Type: api.TypeA, Data: "10.9.9.9"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveClusterZone(ctx, "admin", "prod", "c.example"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing a zone the cluster does not have: %v", err)
	}
	n, err := s.RemoveClusterZone(ctx, "admin", "prod", "b.example")
	if err != nil || n != 1 {
		t.Fatalf("remove zone: %d %v", n, err)
	}
	recs, _ = s.ListRecords(ctx, "b.example")
	if len(recs) != 1 || recs[0].Owner != api.OwnerManual {
		t.Fatalf("records in b after removal: %+v", recs)
	}
	recs, _ = s.ListRecords(ctx, "a.example")
	if len(recs) != 1 {
		t.Fatalf("records in a after removal of b: %+v", recs)
	}
	if _, err := s.ReplaceOwner(ctx, "cluster:prod", "prod", owner, []api.Record{
		{Name: "web.b.example", Type: api.TypeA, Data: "10.0.0.1"},
	}); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("write into a removed zone: %v", err)
	}
	if _, err := s.RemoveClusterZone(ctx, "admin", "prod", "d.example"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveClusterZone(ctx, "admin", "prod", "a.example"); !errors.Is(err, api.ErrInvalid) {
		t.Fatalf("removing the last zone: %v", err)
	}

	// A zone of a cluster cannot be deleted; deleting the cluster frees it.
	var ce *ConflictError
	if err := s.DeleteZone(ctx, "admin", "a.example"); !errors.As(err, &ce) || ce.Conflicts[0].HeldBy != "cluster:prod" {
		t.Fatalf("delete zone held by cluster: %v", err)
	}
	if err := s.DeleteCluster(ctx, "admin", "prod"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCluster(ctx, "admin", "prod"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if err := s.DeleteZone(ctx, "admin", "a.example"); err != nil {
		t.Fatal(err)
	}
	cs, _ := s.ListClusters(ctx)
	if len(cs) != 1 || cs[0].ID != "stage" || strings.Join(cs[0].Zones, ",") != "c.example." {
		t.Fatalf("clusters: %+v", cs)
	}
}

func serials(t *testing.T, s *Store) map[string]uint32 {
	t.Helper()
	zones, err := s.ListZones(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]uint32{}
	for _, z := range zones {
		out[z.Name] = z.Serial
	}
	return out
}

// TestMigrateV1 opens a database created by horus v0.1 (schema version 1)
// and checks that clusters keep their zone and token.
func TestMigrateV1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", DSN(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		schemaV1,
		`PRAGMA user_version = 1`,
		`INSERT INTO zones (id, name, created_at, updated_at) VALUES (1, 'a.example.', 1, 1), (2, 'b.example.', 1, 1)`,
		`INSERT INTO clusters (id, zone_id, token_hash, created_at, last_seen) VALUES ('prod', 1, x'01', 5, 7), ('stage', 2, x'02', 6, NULL)`,
		`INSERT INTO records (zone_id, name, type, data, owner, created_at, updated_at)
		 VALUES (1, 'web.a.example.', 'A', '10.0.0.1', 'prod/default/web', 1, 1)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := Open(ctx, "sqlite", DSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil || v != 2 {
		t.Fatalf("user_version %d %v", v, err)
	}
	cs, err := s.ListClusters(ctx)
	if err != nil || len(cs) != 2 {
		t.Fatalf("clusters: %+v %v", cs, err)
	}
	if cs[0].ID != "prod" || strings.Join(cs[0].Zones, ",") != "a.example." || cs[0].CreatedAt != 5 || cs[0].LastSeen != 7 {
		t.Fatalf("prod after migration: %+v", cs[0])
	}
	if cs[1].ID != "stage" || strings.Join(cs[1].Zones, ",") != "b.example." {
		t.Fatalf("stage after migration: %+v", cs[1])
	}
	c, err := s.ClusterByTokenHash(ctx, []byte{0x01})
	if err != nil || c.ID != "prod" {
		t.Fatalf("token after migration: %+v %v", c, err)
	}
	// The cluster can be extended, and the old records are still its own.
	if _, err := s.AddClusterZone(ctx, "admin", "prod", "c.example"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown zone: %v", err)
	}
	resp, err := s.ReplaceOwner(ctx, "cluster:prod", "prod", "prod/default/web",
		[]api.Record{{Name: "web.a.example.", Type: api.TypeA, Data: "10.0.0.1"}})
	if err != nil || resp.Changed() {
		t.Fatalf("replace after migration: %+v %v", resp, err)
	}
	// Foreign keys still hold: deleting the cluster drops its zone links.
	if err := s.DeleteCluster(ctx, "admin", "stage"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteZone(ctx, "admin", "b.example."); err != nil {
		t.Fatal(err)
	}
	var fk int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_foreign_key_check").Scan(&fk); err != nil || fk != 0 {
		t.Fatalf("foreign key violations: %d %v", fk, err)
	}
}
