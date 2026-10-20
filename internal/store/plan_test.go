package store

import (
	"testing"

	"github.com/warewave/horus/pkg/api"
)

func rec(id int64, name string, typ api.RecordType, data, owner string, ttl uint32) api.Record {
	return api.Record{ID: id, Name: name, Type: typ, Data: data, Owner: owner, TTL: ttl}
}

func TestPlanReplace(t *testing.T) {
	const me = "prod/default/web"

	t.Run("fresh insert", func(t *testing.T) {
		desired := []api.Record{rec(0, "a.z.", api.TypeA, "10.0.0.1", me, 0)}
		p := planReplace(me, desired, nil, nil)
		if len(p.insert) != 1 || len(p.delete)+len(p.update)+len(p.conflicts) != 0 {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("no-op is idempotent", func(t *testing.T) {
		ex := rec(1, "a.z.", api.TypeA, "10.0.0.1", me, 0)
		desired := []api.Record{rec(0, "a.z.", api.TypeA, "10.0.0.1", me, 0)}
		p := planReplace(me, desired, []api.Record{ex}, map[string][]api.Record{"a.z.": {ex}})
		if len(p.insert)+len(p.update)+len(p.delete)+len(p.conflicts) != 0 {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("ip change, ttl change, removed host", func(t *testing.T) {
		old1 := rec(1, "a.z.", api.TypeA, "10.0.0.1", me, 0)
		old2 := rec(2, "b.z.", api.TypeA, "10.0.0.1", me, 0)
		old3 := rec(3, "c.z.", api.TypeA, "10.0.0.1", me, 0)
		desired := []api.Record{
			rec(0, "a.z.", api.TypeA, "10.0.0.2", me, 0),  // ip change
			rec(0, "b.z.", api.TypeA, "10.0.0.1", me, 60), // ttl change
		}
		owned := []api.Record{old1, old2, old3}
		byName := map[string][]api.Record{"a.z.": {old1}, "b.z.": {old2}}
		p := planReplace(me, desired, owned, byName)
		if len(p.insert) != 1 || p.insert[0].Data != "10.0.0.2" {
			t.Fatalf("insert: %+v", p.insert)
		}
		if len(p.update) != 1 || p.update[0].ID != 2 || p.update[0].TTL != 60 {
			t.Fatalf("update: %+v", p.update)
		}
		if len(p.delete) != 2 { // old a + c
			t.Fatalf("delete: %+v", p.delete)
		}
	})

	t.Run("first-wins: conflicting name skipped, others applied", func(t *testing.T) {
		manual := rec(9, "taken.z.", api.TypeA, "10.9.9.9", api.OwnerManual, 0)
		other := rec(8, "other.z.", api.TypeCNAME, "x.example.", "prod/ns2/ing2", 0)
		desired := []api.Record{
			rec(0, "taken.z.", api.TypeA, "10.0.0.1", me, 0),
			rec(0, "other.z.", api.TypeA, "10.0.0.1", me, 0),
			rec(0, "free.z.", api.TypeA, "10.0.0.1", me, 0),
		}
		byName := map[string][]api.Record{"taken.z.": {manual}, "other.z.": {other}}
		p := planReplace(me, desired, nil, byName)
		if len(p.conflicts) != 2 {
			t.Fatalf("conflicts: %+v", p.conflicts)
		}
		if p.conflicts[0].Name != "other.z." || p.conflicts[0].HeldBy != "prod/ns2/ing2" ||
			p.conflicts[1].Name != "taken.z." || p.conflicts[1].HeldBy != api.OwnerManual {
			t.Fatalf("conflicts not sorted/attributed: %+v", p.conflicts)
		}
		if len(p.insert) != 1 || p.insert[0].Name != "free.z." {
			t.Fatalf("insert: %+v", p.insert)
		}
	})

	t.Run("A to CNAME switch", func(t *testing.T) {
		old := rec(1, "a.z.", api.TypeA, "10.0.0.1", me, 0)
		desired := []api.Record{rec(0, "a.z.", api.TypeCNAME, "lb.example.", me, 0)}
		p := planReplace(me, desired, []api.Record{old}, map[string][]api.Record{"a.z.": {old}})
		if len(p.delete) != 1 || len(p.insert) != 1 || len(p.conflicts) != 0 {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("empty desired removes everything", func(t *testing.T) {
		owned := []api.Record{rec(1, "a.z.", api.TypeA, "10.0.0.1", me, 0), rec(2, "b.z.", api.TypeA, "10.0.0.1", me, 0)}
		p := planReplace(me, nil, owned, nil)
		if len(p.delete) != 2 {
			t.Fatalf("%+v", p)
		}
	})
}
