package zone

import (
	"fmt"
	"testing"

	"github.com/Warewave-Technology/horus/pkg/api"
)

func testSnapshot() *Snapshot {
	zones := []api.Zone{
		{Name: "prod.internal.example.", DefaultTTL: 300},
		{Name: "staging.internal.example.", DefaultTTL: 60},
	}
	recs := []api.Record{
		{Zone: "prod.internal.example.", Name: "app.prod.internal.example.", Type: api.TypeA, Data: "10.0.0.1"},
		{Zone: "prod.internal.example.", Name: "app.prod.internal.example.", Type: api.TypeA, Data: "10.0.0.2", TTL: 30},
		{Zone: "prod.internal.example.", Name: "app.prod.internal.example.", Type: api.TypeAAAA, Data: "2001:db8::1"},
		{Zone: "prod.internal.example.", Name: "www.prod.internal.example.", Type: api.TypeCNAME, Data: "app.prod.internal.example."},
		{Zone: "prod.internal.example.", Name: "ext.prod.internal.example.", Type: api.TypeCNAME, Data: "lb.cloud.example.com."},
		{Zone: "prod.internal.example.", Name: "cross.prod.internal.example.", Type: api.TypeCNAME, Data: "api.staging.internal.example."},
		{Zone: "prod.internal.example.", Name: "dangling.prod.internal.example.", Type: api.TypeCNAME, Data: "nope.prod.internal.example."},
		{Zone: "prod.internal.example.", Name: "loop1.prod.internal.example.", Type: api.TypeCNAME, Data: "loop2.prod.internal.example."},
		{Zone: "prod.internal.example.", Name: "loop2.prod.internal.example.", Type: api.TypeCNAME, Data: "loop1.prod.internal.example."},
		{Zone: "prod.internal.example.", Name: "x.deep.prod.internal.example.", Type: api.TypeA, Data: "10.0.0.9"},
		{Zone: "prod.internal.example.", Name: "crossmiss.prod.internal.example.", Type: api.TypeCNAME, Data: "nope.staging.internal.example."},
		{Zone: "staging.internal.example.", Name: "api.staging.internal.example.", Type: api.TypeA, Data: "10.1.0.1"},
		{Zone: "unknown.example.", Name: "orphan.unknown.example.", Type: api.TypeA, Data: "10.9.9.9"},
	}
	return Build(zones, recs)
}

func TestLookup(t *testing.T) {
	s := testSnapshot()

	t.Run("outside every zone is forwarded", func(t *testing.T) {
		r := s.Lookup("google.com.", api.QTypeA)
		if !r.Forward || r.Zone != nil {
			t.Fatalf("got %+v", r)
		}
		// Parent of zones, not inside any.
		if r := s.Lookup("internal.example.", api.QTypeA); !r.Forward || r.Zone != nil {
			t.Fatalf("parent of zones: %+v", r)
		}
		if r := s.Lookup("orphan.unknown.example.", api.QTypeA); !r.Forward || r.Zone != nil {
			t.Fatalf("records of unknown zones must be dropped: %+v", r)
		}
	})

	t.Run("A with default and explicit ttl, case-insensitive", func(t *testing.T) {
		r := s.Lookup("APP.prod.internal.example", api.QTypeA)
		if r.Forward || r.NoData || len(r.Answer) != 2 {
			t.Fatalf("got %+v", r)
		}
		if r.Answer[0].TTL != 300 || r.Answer[1].TTL != 30 {
			t.Fatalf("ttls: %d %d", r.Answer[0].TTL, r.Answer[1].TTL)
		}
	})

	t.Run("AAAA", func(t *testing.T) {
		r := s.Lookup("app.prod.internal.example.", api.QTypeAAAA)
		if len(r.Answer) != 1 || r.Answer[0].Data != "2001:db8::1" {
			t.Fatalf("got %+v", r)
		}
	})

	t.Run("a name we hold without that type is NODATA, not forwarded", func(t *testing.T) {
		r := s.Lookup("app.prod.internal.example.", 16) // TXT
		if r.Forward || !r.NoData || len(r.Answer) != 0 || r.Zone.Name != "prod.internal.example." {
			t.Fatalf("got %+v", r)
		}
		if s.Lookup("app.prod.internal.example.", api.QTypeA).NoData {
			t.Fatal("positive answer flagged NoData")
		}
	})

	t.Run("a name we do not hold inside a zone is forwarded", func(t *testing.T) {
		r := s.Lookup("missing.prod.internal.example.", api.QTypeA)
		if !r.Forward || r.Zone == nil || r.Zone.Name != "prod.internal.example." || len(r.Answer) != 0 {
			t.Fatalf("got %+v", r)
		}
	})

	t.Run("empty non-terminal and zone apex without records are forwarded", func(t *testing.T) {
		for _, name := range []string{"deep.prod.internal.example.", "prod.internal.example."} {
			for _, qt := range []uint16{api.QTypeA, api.QTypeSOA} {
				if r := s.Lookup(name, qt); !r.Forward || r.Zone == nil {
					t.Fatalf("%s %d: got %+v", name, qt, r)
				}
			}
		}
	})

	t.Run("NODATA at the end of an in-zone CNAME chain", func(t *testing.T) {
		r := s.Lookup("www.prod.internal.example.", 16) // TXT
		if r.Forward || !r.NoData || len(r.Answer) != 1 || r.Answer[0].Type != api.TypeCNAME {
			t.Fatalf("got %+v", r)
		}
	})

	t.Run("in-zone CNAME chase", func(t *testing.T) {
		r := s.Lookup("www.prod.internal.example.", api.QTypeA)
		if len(r.Answer) != 3 || r.Answer[0].Type != api.TypeCNAME || r.Answer[1].Type != api.TypeA {
			t.Fatalf("got %+v", r.Answer)
		}
		if r.ChaseTarget != "" || r.Forward {
			t.Fatal("no upstream needed")
		}
	})

	t.Run("CNAME query returns only CNAME", func(t *testing.T) {
		r := s.Lookup("www.prod.internal.example.", api.QTypeCNAME)
		if len(r.Answer) != 1 || r.Answer[0].Type != api.TypeCNAME {
			t.Fatalf("got %+v", r.Answer)
		}
	})

	t.Run("cross-zone CNAME chase", func(t *testing.T) {
		r := s.Lookup("cross.prod.internal.example.", api.QTypeA)
		if len(r.Answer) != 2 || r.Answer[1].Data != "10.1.0.1" || r.Answer[1].TTL != 60 {
			t.Fatalf("got %+v", r.Answer)
		}
	})

	t.Run("external CNAME needs upstream chase", func(t *testing.T) {
		r := s.Lookup("ext.prod.internal.example.", api.QTypeA)
		if r.Forward || len(r.Answer) != 1 || r.ChaseTarget != "lb.cloud.example.com." {
			t.Fatalf("got %+v", r)
		}
	})

	t.Run("CNAME to a name we do not hold is chased upstream, also inside our zones", func(t *testing.T) {
		r := s.Lookup("dangling.prod.internal.example.", api.QTypeA)
		if r.Forward || len(r.Answer) != 1 || r.ChaseTarget != "nope.prod.internal.example." {
			t.Fatalf("got %+v", r)
		}
		r = s.Lookup("crossmiss.prod.internal.example.", api.QTypeA)
		if len(r.Answer) != 1 || r.ChaseTarget != "nope.staging.internal.example." {
			t.Fatalf("cross-zone: %+v", r)
		}
	})

	t.Run("NODATA uses the zone of the last name", func(t *testing.T) {
		r := s.Lookup("cross.prod.internal.example.", api.QTypeAAAA)
		if !r.NoData || len(r.Answer) != 1 || r.Zone.Name != "staging.internal.example." {
			t.Fatalf("got %+v", r)
		}
	})

	t.Run("CNAME loop terminates without repeating records", func(t *testing.T) {
		r := s.Lookup("loop1.prod.internal.example.", api.QTypeA)
		if len(r.Answer) != 2 || r.Answer[0].Name != "loop1.prod.internal.example." ||
			r.Answer[1].Name != "loop2.prod.internal.example." || r.Forward || r.ChaseTarget != "" {
			t.Fatalf("got %+v", r)
		}
	})

	t.Run("long CNAME chain is cut at maxCNAMEChain", func(t *testing.T) {
		z := api.Zone{Name: "c.example.", DefaultTTL: 60}
		var recs []api.Record
		for i := 0; i < 20; i++ {
			recs = append(recs, api.Record{Zone: z.Name, Name: fmt.Sprintf("n%d.c.example.", i),
				Type: api.TypeCNAME, Data: fmt.Sprintf("n%d.c.example.", i+1)})
		}
		r := Build([]api.Zone{z}, recs).Lookup("n0.c.example.", api.QTypeA)
		if len(r.Answer) != maxCNAMEChain || r.NoData || r.Forward || r.ChaseTarget != "" {
			t.Fatalf("got %d answers: %+v", len(r.Answer), r)
		}
	})
}

func TestHolder(t *testing.T) {
	h := NewHolder()
	if h.Load() == nil || h.Load().Len() != 0 {
		t.Fatal("empty holder must have an empty snapshot")
	}
	h.Store(testSnapshot())
	if h.Load().Len() != 12 || h.Load().Zones() != 2 {
		t.Fatalf("len=%d zones=%d", h.Load().Len(), h.Load().Zones())
	}
}
