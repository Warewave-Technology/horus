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

	"github.com/warewave/horus/pkg/api"
)

// maxCNAMEChain bounds in-zone CNAME chasing (loops are possible: a -> b -> a).
const maxCNAMEChain = 8

// Rcode is the subset of DNS response codes the snapshot can decide on its own.
type Rcode int

const (
	NoError Rcode = iota
	NXDomain
)

// Result is the outcome of a lookup.
type Result struct {
	// Authoritative is false when the name is not inside any of our zones;
	// the caller should forward the query upstream.
	Authoritative bool
	Rcode         Rcode
	// Zone is the zone that answered (for building the SOA). Set when Authoritative.
	Zone *api.Zone
	// Answer holds the records to return, in order (CNAME chain first).
	// TTLs are already resolved (record TTL or zone default).
	Answer []api.Record
	// ChaseTarget is set when the last CNAME in Answer points outside our zones.
	// The caller should resolve ChaseTarget upstream and append the result.
	ChaseTarget string
	// SOA is true when the answer for qtype SOA at the apex should be synthesized.
	SOA bool
}

type node struct {
	zone    *api.Zone
	records []api.Record // all records of this name
}

// Snapshot is an immutable index of zones and records.
type Snapshot struct {
	zones map[string]*api.Zone // by name
	names map[string]*node     // by owner name
	// ents holds empty non-terminals: names that have no records themselves
	// but have descendants that do (they must answer NODATA, not NXDOMAIN).
	ents     map[string]bool
	nRecords int
}

// Build creates a Snapshot. Records whose zone is unknown are ignored.
func Build(zones []api.Zone, records []api.Record) *Snapshot {
	s := &Snapshot{
		zones: make(map[string]*api.Zone, len(zones)),
		names: make(map[string]*node, len(records)),
		ents:  map[string]bool{},
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
		// Mark every ancestor between the name and its zone apex as an ENT.
		for p := parent(r.Name); p != "" && p != z.Name && api.InZone(p, z.Name); p = parent(p) {
			s.ents[p] = true
		}
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

// Lookup resolves qname/qtype against authoritative data. qname may be in any
// case and with or without the trailing dot.
func (s *Snapshot) Lookup(qname string, qtype uint16) Result {
	qname = api.NormalizeName(qname)
	z := s.FindZone(qname)
	if z == nil {
		return Result{}
	}
	res := Result{Authoritative: true, Zone: z}
	name := qname
	for i := 0; i <= maxCNAMEChain; i++ {
		n := s.names[name]
		if n == nil {
			if name == z.Name || s.ents[name] {
				// Apex with no records, or an empty non-terminal.
				if name == qname && qtype == api.QTypeSOA && name == z.Name {
					res.SOA = true
				}
				return res
			}
			// Either qname itself does not exist, or an in-zone CNAME target
			// is dangling. Per RFC 6604 the rcode reflects the last name in
			// the chain, so both cases are NXDOMAIN (with any CNAMEs kept).
			res.Rcode = NXDomain
			return res
		}

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
			if !matched && name == qname && qtype == api.QTypeSOA && name == z.Name {
				res.SOA = true
			}
			return res
		}

		// Follow the CNAME.
		res.Answer = append(res.Answer, *cname)
		target := cname.Data
		tz := s.FindZone(target)
		if tz == nil {
			res.ChaseTarget = target
			return res
		}
		name = target
		z = tz
	}
	// Chain too long or a loop: return what we collected.
	return res
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
