package zone

import (
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
		{Zone: "staging.internal.example.", Name: "api.staging.internal.example.", Type: api.TypeA, Data: "10.1.0.1"},
		{Zone: "unknown.example.", Name: "orphan.unknown.example.", Type: api.TypeA, Data: "10.9.9.9"},
	}
	return Build(zones, recs)
}

func TestLookup(t *testing.T) {
	s := testSnapshot()

	t.Run("not ours", func(t *testing.T) {
		r := s.Lookup("google.com.", api.QTypeA)
		if r.Authoritative {
			t.Fatal("expected non-authoritative")
		}
		// Sibling of a zone, not inside it.
		if s.Lookup("internal.example.", api.QTypeA).Authoritative {
			t.Fatal("parent of zones is not ours")
		}
		if s.Lookup("orphan.unknown.example.", api.QTypeA).Authoritative {
			t.Fatal("records of unknown zones must be dropped")
		}
	})

	t.Run("A with default and explicit ttl, case-insensitive", func(t *testing.T) {
		r := s.Lookup("APP.prod.internal.example", api.QTypeA)
		if !r.Authoritative || r.Rcode != NoError || len(r.Answer) != 2 {
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

	t.Run("NODATA", func(t *testing.T) {
		r := s.Lookup("app.prod.internal.example.", 16) // TXT
		if !r.Authoritative || r.Rcode != NoError || len(r.Answer) != 0 {
			t.Fatalf("got %+v", r)
		}
	})

	t.Run("NXDOMAIN", func(t *testing.T) {
		r := s.Lookup("missing.prod.internal.example.", api.QTypeA)
		if !r.Authoritative || r.Rcode != NXDomain || r.Zone.Name != "prod.internal.example." {
			t.Fatalf("got %+v", r)
		}
	})

	t.Run("empty non-terminal is NODATA", func(t *testing.T) {
		r := s.Lookup("deep.prod.internal.example.", api.QTypeA)
		if r.Rcode != NoError || len(r.Answer) != 0 {
			t.Fatalf("got %+v", r)
		}
	})

	t.Run("apex SOA", func(t *testing.T) {
		r := s.Lookup("prod.internal.example.", api.QTypeSOA)
		if !r.SOA || r.Rcode != NoError {
			t.Fatalf("got %+v", r)
		}
		if s.Lookup("app.prod.internal.example.", api.QTypeSOA).SOA {
			t.Fatal("SOA only at apex")
		}
	})

	t.Run("in-zone CNAME chase", func(t *testing.T) {
		r := s.Lookup("www.prod.internal.example.", api.QTypeA)
		if len(r.Answer) != 3 || r.Answer[0].Type != api.TypeCNAME || r.Answer[1].Type != api.TypeA {
			t.Fatalf("got %+v", r.Answer)
		}
		if r.ChaseTarget != "" {
			t.Fatal("no upstream chase expected")
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
		if len(r.Answer) != 1 || r.ChaseTarget != "lb.cloud.example.com." {
			t.Fatalf("got %+v", r)
		}
	})

	t.Run("dangling CNAME is NXDOMAIN with chain", func(t *testing.T) {
		r := s.Lookup("dangling.prod.internal.example.", api.QTypeA)
		if r.Rcode != NXDomain || len(r.Answer) != 1 {
			t.Fatalf("got %+v", r)
		}
	})

	t.Run("CNAME loop terminates", func(t *testing.T) {
		r := s.Lookup("loop1.prod.internal.example.", api.QTypeA)
		if len(r.Answer) == 0 || len(r.Answer) > maxCNAMEChain+1 {
			t.Fatalf("got %d answers", len(r.Answer))
		}
	})
}

func TestHolder(t *testing.T) {
	h := NewHolder()
	if h.Load() == nil || h.Load().Len() != 0 {
		t.Fatal("empty holder must have an empty snapshot")
	}
	h.Store(testSnapshot())
	if h.Load().Len() != 11 || h.Load().Zones() != 2 {
		t.Fatalf("len=%d zones=%d", h.Load().Len(), h.Load().Zones())
	}
}
