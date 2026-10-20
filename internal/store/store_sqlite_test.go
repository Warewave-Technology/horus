package store

// Integration tests against a real SQLite database (modernc.org/sqlite).

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/warewave/horus/pkg/api"
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
	if _, err := s.CreateCluster(ctx, "admin", "prod", z.Name, []byte("hash-1")); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClusterByTokenHash(ctx, []byte("hash-1"))
	if err != nil || c.ID != "prod" || c.Zone != z.Name {
		t.Fatalf("auth: %+v %v", c, err)
	}

	owner := "prod/default/web"
	resp, err := s.ReplaceOwner(ctx, "cluster:prod", z.Name, owner, []api.Record{
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
	resp, err = s.ReplaceOwner(ctx, "cluster:prod", z.Name, owner, []api.Record{
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
