package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Warewave-Technology/horus/internal/store"
	"github.com/Warewave-Technology/horus/pkg/api"
	"github.com/Warewave-Technology/horus/pkg/client"
)

// fakeStore is an in-memory Store good enough to exercise routing, auth and
// error mapping. Ownership/diff rules are tested in internal/store.
type fakeStore struct {
	mu       sync.Mutex
	zones    map[string]api.Zone
	records  []api.Record
	clusters map[string]api.Cluster
	tokens   map[string]string // hash -> cluster id
	nextID   int64
}

func newFake() *fakeStore {
	return &fakeStore{zones: map[string]api.Zone{}, clusters: map[string]api.Cluster{}, tokens: map[string]string{}}
}

func (f *fakeStore) ListZones(context.Context) ([]api.Zone, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []api.Zone
	for _, z := range f.zones {
		out = append(out, z)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeStore) CreateZone(_ context.Context, _ string, z api.Zone) (api.Zone, error) {
	if err := api.ValidateZone(&z); err != nil {
		return z, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.zones[z.Name]; ok {
		return z, store.ErrExists
	}
	f.zones[z.Name] = z
	return z, nil
}

func (f *fakeStore) DeleteZone(_ context.Context, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	name = api.NormalizeName(name)
	if _, ok := f.zones[name]; !ok {
		return store.ErrNotFound
	}
	delete(f.zones, name)
	return nil
}

func (f *fakeStore) ListRecords(_ context.Context, zone string) ([]api.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	zone = api.NormalizeName(zone)
	if _, ok := f.zones[zone]; !ok {
		return nil, store.ErrNotFound
	}
	var out []api.Record
	for _, r := range f.records {
		if r.Zone == zone {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeStore) holder(name, except string) string {
	for _, r := range f.records {
		if r.Name == name && r.Owner != except {
			return r.Owner
		}
	}
	return ""
}

func (f *fakeStore) AddRecord(_ context.Context, _ string, r api.Record) (api.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	zone := api.NormalizeName(r.Zone)
	if _, ok := f.zones[zone]; !ok {
		return r, store.ErrNotFound
	}
	if err := api.ValidateRecord(&r, zone); err != nil {
		return r, err
	}
	if h := f.holder(r.Name, api.OwnerManual); h != "" {
		return r, &store.ConflictError{Conflicts: []api.Conflict{{Name: r.Name, HeldBy: h}}}
	}
	f.nextID++
	r.ID, r.Zone, r.Owner = f.nextID, zone, api.OwnerManual
	f.records = append(f.records, r)
	return r, nil
}

func (f *fakeStore) DeleteRecord(_ context.Context, _ string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.records {
		if r.ID == id {
			f.records = append(f.records[:i], f.records[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeStore) ReplaceOwner(_ context.Context, _, zone, owner string, recs []api.Record) (api.OwnerRecordsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var resp api.OwnerRecordsResponse
	for i := range recs {
		if err := api.ValidateRecord(&recs[i], zone); err != nil {
			return resp, err
		}
	}
	kept := f.records[:0]
	for _, r := range f.records {
		if r.Owner == owner {
			resp.Removed++
			continue
		}
		kept = append(kept, r)
	}
	f.records = kept
	for _, r := range recs {
		if h := f.holder(r.Name, owner); h != "" {
			resp.Conflicts = append(resp.Conflicts, api.Conflict{Name: r.Name, HeldBy: h})
			continue
		}
		f.nextID++
		r.ID, r.Zone, r.Owner = f.nextID, zone, owner
		f.records = append(f.records, r)
		resp.Added++
	}
	return resp, nil
}

func (f *fakeStore) DeleteOwner(_ context.Context, _, owner string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	kept := f.records[:0]
	for _, r := range f.records {
		if r.Owner == owner {
			n++
			continue
		}
		kept = append(kept, r)
	}
	f.records = kept
	return n, nil
}

func (f *fakeStore) ListOwners(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, r := range f.records {
		if strings.HasPrefix(r.Owner, prefix) && !seen[r.Owner] {
			seen[r.Owner] = true
			out = append(out, r.Owner)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeStore) ListClusters(context.Context) ([]api.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []api.Cluster
	for _, c := range f.clusters {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeStore) CreateCluster(_ context.Context, _, id, zone string, h []byte) (api.Cluster, error) {
	if err := api.ValidateClusterID(id); err != nil {
		return api.Cluster{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	zone = api.NormalizeName(zone)
	if _, ok := f.zones[zone]; !ok {
		return api.Cluster{}, store.ErrNotFound
	}
	c := api.Cluster{ID: id, Zone: zone}
	f.clusters[id] = c
	f.tokens[string(h)] = id
	return c, nil
}

func (f *fakeStore) RotateClusterToken(_ context.Context, _, id string, h []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.clusters[id]; !ok {
		return store.ErrNotFound
	}
	for k, v := range f.tokens {
		if v == id {
			delete(f.tokens, k)
		}
	}
	f.tokens[string(h)] = id
	return nil
}

func (f *fakeStore) DeleteCluster(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.clusters, id)
	return nil
}

func (f *fakeStore) ClusterByTokenHash(_ context.Context, h []byte) (api.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.tokens[string(h)]
	if !ok {
		return api.Cluster{}, store.ErrNotFound
	}
	return f.clusters[id], nil
}

func (f *fakeStore) Backup(context.Context, string) error { return nil }

const adminTok = "admin-token-admin-token-admin-token-1234"

func setup(t *testing.T) (*client.Client, *httptest.Server, *int) {
	t.Helper()
	reloads := 0
	srv := New(Options{
		Store:      newFake(),
		AdminToken: adminTok,
		Reload:     func(context.Context) error { reloads++; return nil },
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c, err := client.New(ts.URL, adminTok, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return c, ts, &reloads
}

func TestAdminFlow(t *testing.T) {
	ctx := context.Background()
	c, ts, reloads := setup(t)

	if w, err := c.WhoAmI(ctx); err != nil || !w.Admin {
		t.Fatalf("whoami: %+v %v", w, err)
	}
	if _, err := c.CreateZone(ctx, "prod.internal.example", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateZone(ctx, "prod.internal.example", 0); !client.IsStatus(err, http.StatusConflict) {
		t.Fatalf("duplicate zone: %v", err)
	}
	if _, err := c.AddRecord(ctx, "prod.internal.example.", api.Record{Name: "db.prod.internal.example", Type: api.TypeA, Data: "nope"}); !client.IsStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("invalid record: %v", err)
	}
	rec, err := c.AddRecord(ctx, "prod.internal.example.", api.Record{Name: "db.prod.internal.example", Type: api.TypeA, Data: "10.0.0.5"})
	if err != nil || rec.ID == 0 {
		t.Fatalf("add: %+v %v", rec, err)
	}
	if _, err := c.ListRecords(ctx, "missing.example."); !client.IsStatus(err, http.StatusNotFound) {
		t.Fatalf("missing zone: %v", err)
	}
	if *reloads != 2 {
		t.Fatalf("expected 2 reloads (zone + record), got %d", *reloads)
	}

	// Unknown JSON fields are rejected.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/zones", bytes.NewBufferString(`{"name":"x.example.","bogus":1}`))
	req.Header.Set("Authorization", "Bearer "+adminTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown field: %v %v", resp.StatusCode, err)
	}
}

func TestAuth(t *testing.T) {
	ctx := context.Background()
	_, ts, _ := setup(t)
	for _, tok := range []string{"", "wrong"} {
		c, _ := client.New(ts.URL, tok, client.Options{})
		if _, err := c.ListZones(ctx); !client.IsStatus(err, http.StatusUnauthorized) {
			t.Fatalf("token %q: %v", tok, err)
		}
	}
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz must be public: %v", err)
	}
}

func TestClusterFlow(t *testing.T) {
	ctx := context.Background()
	admin, ts, _ := setup(t)
	if _, err := admin.CreateZone(ctx, "prod.internal.example.", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.AddRecord(ctx, "prod.internal.example.", api.Record{Name: "taken.prod.internal.example.", Type: api.TypeA, Data: "10.0.0.9"}); err != nil {
		t.Fatal(err)
	}
	cl, err := admin.CreateCluster(ctx, "prod", "prod.internal.example")
	if err != nil || !strings.HasPrefix(cl.Token, "hrs_") {
		t.Fatalf("create cluster: %+v %v", cl, err)
	}
	op, _ := client.New(ts.URL, cl.Token, client.Options{})

	w, err := op.WhoAmI(ctx)
	if err != nil || w.Admin || w.Cluster != "prod" || w.Zone != "prod.internal.example." {
		t.Fatalf("whoami: %+v %v", w, err)
	}
	// Cluster tokens cannot use admin endpoints.
	if _, err := op.ListZones(ctx); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("cluster on admin endpoint: %v", err)
	}
	// Admin token cannot use owner endpoints.
	if _, err := admin.ListOwners(ctx); !client.IsStatus(err, http.StatusForbidden) {
		t.Fatalf("admin on owner endpoint: %v", err)
	}

	resp, err := op.ReplaceOwner(ctx, "default", "web", []api.Record{
		{Name: "web.prod.internal.example.", Type: api.TypeA, Data: "10.0.1.1"},
		{Name: "taken.prod.internal.example.", Type: api.TypeA, Data: "10.0.1.1"},
	})
	if err != nil || resp.Added != 1 || len(resp.Conflicts) != 1 {
		t.Fatalf("replace: %+v %v", resp, err)
	}
	// Out-of-zone records are a validation error.
	if _, err := op.ReplaceOwner(ctx, "default", "web", []api.Record{
		{Name: "web.staging.internal.example.", Type: api.TypeA, Data: "10.0.1.1"},
	}); !client.IsStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("out of zone: %v", err)
	}
	// Invalid namespace in path.
	if _, err := op.ReplaceOwner(ctx, "Bad_NS", "web", nil); !client.IsStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("bad ns: %v", err)
	}

	owners, err := op.ListOwners(ctx)
	if err != nil || len(owners) != 1 || owners[0] != "default/web" {
		t.Fatalf("owners: %v %v", owners, err)
	}
	if err := op.DeleteOwner(ctx, "default", "web"); err != nil {
		t.Fatal(err)
	}
	if owners, _ := op.ListOwners(ctx); len(owners) != 0 {
		t.Fatalf("owners after delete: %v", owners)
	}

	// Rotation invalidates the old token.
	newTok, err := admin.RotateClusterToken(ctx, "prod")
	if err != nil || newTok == cl.Token {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := op.WhoAmI(ctx); !client.IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("old token still valid: %v", err)
	}
}
