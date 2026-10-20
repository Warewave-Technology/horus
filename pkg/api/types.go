// Package api holds the wire types shared by the horus server, the CLI and the
// Kubernetes operator, plus the validation rules every record must pass.
//
// It must stay dependency-free (stdlib only) so the operator module can import
// it without pulling in server dependencies.
package api

// RecordType is a DNS record type supported by horus.
type RecordType string

const (
	TypeA     RecordType = "A"
	TypeAAAA  RecordType = "AAAA"
	TypeCNAME RecordType = "CNAME"
)

// DNS wire values for the types horus understands. Kept here so packages that
// do not import a DNS library (snapshot, tests) can still talk about qtypes.
const (
	QTypeA     uint16 = 1
	QTypeCNAME uint16 = 5
	QTypeSOA   uint16 = 6
	QTypeAAAA  uint16 = 28
	QTypeANY   uint16 = 255
)

// QType returns the DNS wire value of the record type.
func (t RecordType) QType() uint16 {
	switch t {
	case TypeA:
		return QTypeA
	case TypeAAAA:
		return QTypeAAAA
	case TypeCNAME:
		return QTypeCNAME
	}
	return 0
}

// OwnerManual is the owner of every record created through the admin API/CLI.
const OwnerManual = "manual"

// Zone is an authoritative zone served by horus.
type Zone struct {
	ID         int64  `json:"id,omitempty"`
	Name       string `json:"name"`                  // lowercase FQDN with trailing dot
	DefaultTTL uint32 `json:"default_ttl,omitempty"` // seconds; 0 on create = server default
	Serial     uint32 `json:"serial,omitempty"`
	CreatedAt  int64  `json:"created_at,omitempty"` // unix seconds
	UpdatedAt  int64  `json:"updated_at,omitempty"`
}

// Record is a single resource record.
type Record struct {
	ID        int64      `json:"id,omitempty"`
	Zone      string     `json:"zone,omitempty"`
	Name      string     `json:"name"`
	Type      RecordType `json:"type"`
	Data      string     `json:"data"`
	TTL       uint32     `json:"ttl,omitempty"` // 0 = zone default
	Owner     string     `json:"owner,omitempty"`
	CreatedAt int64      `json:"created_at,omitempty"`
	UpdatedAt int64      `json:"updated_at,omitempty"`
}

// Key identifies a record independently of its id/ttl/owner.
func (r Record) Key() string { return r.Name + " " + string(r.Type) + " " + r.Data }

// Cluster is a Kubernetes cluster allowed to manage records in exactly one zone.
type Cluster struct {
	ID        string `json:"id"`
	Zone      string `json:"zone"`
	CreatedAt int64  `json:"created_at,omitempty"`
	LastSeen  int64  `json:"last_seen,omitempty"`
	// Token is only ever returned once: on create and on rotate.
	Token string `json:"token,omitempty"`
}

// CreateZoneRequest is the body of POST /v1/zones.
type CreateZoneRequest struct {
	Name       string `json:"name"`
	DefaultTTL uint32 `json:"default_ttl,omitempty"`
}

// CreateClusterRequest is the body of POST /v1/clusters.
type CreateClusterRequest struct {
	ID   string `json:"id"`
	Zone string `json:"zone"`
}

// OwnerRecordsRequest is the body of PUT /v1/owners/{namespace}/{name}/records.
// It is the complete desired set for that owner; anything not listed is removed.
type OwnerRecordsRequest struct {
	Records []Record `json:"records"`
}

// Conflict reports a name that could not be applied because another owner
// already holds it (first-wins).
type Conflict struct {
	Name   string `json:"name"`
	HeldBy string `json:"held_by"`
}

// OwnerRecordsResponse is the result of PUT /v1/owners/{namespace}/{name}/records.
// Non-conflicting names are always applied; conflicting names are skipped and
// listed in Conflicts.
type OwnerRecordsResponse struct {
	Added     int        `json:"added"`
	Updated   int        `json:"updated"`
	Removed   int        `json:"removed"`
	Conflicts []Conflict `json:"conflicts,omitempty"`
}

// Changed reports whether the PUT modified anything.
func (r OwnerRecordsResponse) Changed() bool { return r.Added+r.Updated+r.Removed > 0 }

// OwnersResponse is the body of GET /v1/owners: owners of the calling cluster,
// as "namespace/name".
type OwnersResponse struct {
	Owners []string `json:"owners"`
}

// WhoAmI is the body of GET /v1/whoami.
type WhoAmI struct {
	Admin   bool   `json:"admin"`
	Cluster string `json:"cluster,omitempty"`
	Zone    string `json:"zone,omitempty"`
}

// Error is the body of every non-2xx response.
type Error struct {
	Error string `json:"error"`
	// Conflicts is set on 409 responses caused by first-wins ownership.
	Conflicts []Conflict `json:"conflicts,omitempty"`
}
