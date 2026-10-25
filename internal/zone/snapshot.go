// Package zone holds the in-memory, read-only view of all authoritative data.
//
// The DNS hot path only ever reads a *Snapshot. Writes go to SQLite, after which
// a brand-new Snapshot is built and swapped in atomically (see Holder), so
// lookups never take a lock and never touch the database.
//
// This package deliberately does not import a DNS library: it speaks in
// api.Record and qtype numbers, which keeps the resolution rules unit-testable.
package zone

import (
	"strings"
	"sync/atomic"

	"github.com/Warewave-Technology/horus/pkg/api"
)

// maxCNAMEChain bounds the number of CNAMEs followed inside our zones.
// Loops (a -> b -> a) are detected separately and stop earlier.
const maxCNAMEChain = 8

// Result is the outcome of a lookup.
//
// horus overrides names, it does not own whole zones: a name it holds records
// for is answered from the snapshot; any other name, even inside one of our
// zones, is resolved upstream (Forward). Zones may therefore contain public
// names that horus never heard of.
type Result struct {
	// Forward is true when horus holds no record for qname: the caller
	// resolves the query upstream.
	Forward bool
	// Zone is our zone containing qname, or nil if qname is outside every
	// zone. For answers it is the zone of the last name in the CNAME chain
	// (for the SOA of a NODATA answer).
	Zone *api.Zone
	// Answer holds the records to return, in order (CNAME chain first).
	// TTLs are already resolved (record TTL or zone default).
	Answer []api.Record
	// ChaseTarget is set when the last CNAME in Answer points to a name we
	// hold no record for (outside our zones, or inside them but not ours).
	// The caller resolves ChaseTarget upstream and appends the result.
	ChaseTarget string
	// NoData is true when the last name in the chain is one we hold but it
	// has no data of qtype. The answer carries the SOA of Zone; the query is
	// not forwarded, so internal and public data never mix for one name.
	NoData bool
}

type node struct {
	zone    *api.Zone
	records []api.Record // all records of this name
}

// Snapshot is an immutable index of zones and records.
type Snapshot struct {
	zones    map[string]*api.Zone // by name
	names    map[string]*node     // by owner name
	nRecords int
}

// Build creates a Snapshot. Records whose zone is unknown are ignored.
func Build(zones []api.Zone, records []api.Record) *Snapshot {
	s := &Snapshot{
		zones: make(map[string]*api.Zone, len(zones)),
		names: make(map[string]*node, len(records)),
	}
	for i := range zones {
		z := zones[i]
		s.zones[z.Name] = &z
	}
	for _, r := range records {
		z := s.zones[r.Zone]
		if z == nil {
			continue
		}
		if r.TTL == 0 {
			r.TTL = z.DefaultTTL
		}
		n := s.names[r.Name]
		if n == nil {
			n = &node{zone: z}
			s.names[r.Name] = n
		}
		n.records = append(n.records, r)
		s.nRecords++
	}
	return s
}

// Len returns the number of records in the snapshot.
func (s *Snapshot) Len() int { return s.nRecords }

// Zones returns the number of zones in the snapshot.
func (s *Snapshot) Zones() int { return len(s.zones) }

// FindZone returns the most specific zone containing name, or nil.
func (s *Snapshot) FindZone(name string) *api.Zone {
	for n := name; n != ""; n = parent(n) {
		if z, ok := s.zones[n]; ok {
			return z
		}
	}
	return nil
}

// Lookup resolves qname/qtype against the snapshot. qname may be in any case
// and with or without the trailing dot.
func (s *Snapshot) Lookup(qname string, qtype uint16) Result {
	qname = api.NormalizeName(qname)
	z := s.FindZone(qname)
	n := s.names[qname]
	if n == nil {
		// Not ours (possibly a public name inside one of our zones).
		return Result{Forward: true, Zone: z}
	}
	res := Result{Zone: z}
	name := qname
	visited := map[string]bool{}
	for {
		visited[name] = true
		var cname *api.Record
		matched := false
		for i := range n.records {
			r := n.records[i]
			if r.Type == api.TypeCNAME {
				cname = &n.records[i]
			}
			if qtype == api.QTypeANY || r.Type.QType() == qtype {
				res.Answer = append(res.Answer, r)
				matched = true
			}
		}
		if matched || cname == nil || qtype == api.QTypeCNAME {
			res.NoData = !matched
			return res
		}

		// Follow the CNAME.
		res.Answer = append(res.Answer, *cname)
		target := cname.Data
		next := s.names[target]
		if next == nil {
			res.ChaseTarget = target
			return res
		}
		if visited[target] || len(visited) >= maxCNAMEChain {
			// A loop or an overlong chain: return each CNAME once and let
			// the client's resolver give up on it.
			return res
		}
		name, n = target, next
		res.Zone = next.zone
	}
}

func parent(name string) string {
	i := strings.IndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		return ""
	}
	return name[i+1:]
}

// Holder publishes the current Snapshot to concurrent readers.
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// NewHolder returns a Holder with an empty snapshot.
func NewHolder() *Holder {
	h := &Holder{}
	h.p.Store(Build(nil, nil))
	return h
}

// Load returns the current snapshot. It never returns nil.
func (h *Holder) Load() *Snapshot { return h.p.Load() }

// Store swaps in a new snapshot.
func (h *Holder) Store(s *Snapshot) { h.p.Store(s) }
