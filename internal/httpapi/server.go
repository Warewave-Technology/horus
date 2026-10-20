// Package httpapi is the REST API used by the CLI (admin token) and by the
// Kubernetes operator (per-cluster tokens).
//
// Every successful write is followed by a snapshot reload, so a 2xx response
// means the change is already being served over DNS.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/warewave/horus/internal/store"
	"github.com/warewave/horus/pkg/api"
)

// Store is what the API needs from persistence. *store.Store implements it.
type Store interface {
	ListZones(ctx context.Context) ([]api.Zone, error)
	CreateZone(ctx context.Context, actor string, z api.Zone) (api.Zone, error)
	DeleteZone(ctx context.Context, actor, name string) error
	ListRecords(ctx context.Context, zone string) ([]api.Record, error)
	AddRecord(ctx context.Context, actor string, r api.Record) (api.Record, error)
	DeleteRecord(ctx context.Context, actor string, id int64) error
	ReplaceOwner(ctx context.Context, actor, zone, owner string, recs []api.Record) (api.OwnerRecordsResponse, error)
	DeleteOwner(ctx context.Context, actor, owner string) (int, error)
	ListOwners(ctx context.Context, prefix string) ([]string, error)
	ListClusters(ctx context.Context) ([]api.Cluster, error)
	CreateCluster(ctx context.Context, actor, id, zone string, tokenHash []byte) (api.Cluster, error)
	RotateClusterToken(ctx context.Context, actor, id string, tokenHash []byte) error
	DeleteCluster(ctx context.Context, actor, id string) error
	ClusterByTokenHash(ctx context.Context, tokenHash []byte) (api.Cluster, error)
	Backup(ctx context.Context, path string) error
}

// Options configures the API server.
type Options struct {
	Store      Store
	AdminToken string
	// Reload rebuilds the DNS snapshot from the store after a write.
	Reload    func(ctx context.Context) error
	BackupDir string
	Log       *slog.Logger
	// Extra routes mounted without auth (e.g. /metrics).
	Public map[string]http.Handler
}

// Server is the HTTP API.
type Server struct {
	opt   Options
	admin []byte
	// writeMu serializes write+reload so the snapshot always reflects the
	// latest committed write order.
	writeMu sync.Mutex
}

// New returns an API server.
func New(opt Options) *Server {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Reload == nil {
		opt.Reload = func(context.Context) error { return nil }
	}
	return &Server{opt: opt, admin: []byte(opt.AdminToken)}
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	for path, h := range s.opt.Public {
		mux.Handle("GET "+path, h)
	}

	admin := s.authed(true)
	anyTok := s.authed(false)

	mux.Handle("GET /v1/whoami", anyTok(s.whoami))

	mux.Handle("GET /v1/zones", admin(s.listZones))
	mux.Handle("POST /v1/zones", admin(s.createZone))
	mux.Handle("DELETE /v1/zones/{zone}", admin(s.deleteZone))
	mux.Handle("GET /v1/zones/{zone}/records", admin(s.listRecords))
	mux.Handle("POST /v1/zones/{zone}/records", admin(s.addRecord))
	mux.Handle("DELETE /v1/records/{id}", admin(s.deleteRecord))

	mux.Handle("GET /v1/clusters", admin(s.listClusters))
	mux.Handle("POST /v1/clusters", admin(s.createCluster))
	mux.Handle("POST /v1/clusters/{id}/rotate-token", admin(s.rotateCluster))
	mux.Handle("DELETE /v1/clusters/{id}", admin(s.deleteCluster))

	mux.Handle("POST /v1/backup", admin(s.backup))

	mux.Handle("GET /v1/owners", anyTok(s.listOwners))
	mux.Handle("PUT /v1/owners/{namespace}/{name}/records", anyTok(s.replaceOwner))
	mux.Handle("DELETE /v1/owners/{namespace}/{name}", anyTok(s.deleteOwner))

	return s.logRequests(mux)
}

// ---------------------------------------------------------------- auth

type identity struct {
	admin   bool
	cluster api.Cluster
}

func (id identity) actor() string {
	if id.admin {
		return "admin"
	}
	return "cluster:" + id.cluster.ID
}

type ctxKey struct{}

func idFrom(ctx context.Context) identity { id, _ := ctx.Value(ctxKey{}).(identity); return id }

// HashToken is how cluster tokens are stored.
func HashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

// NewToken returns a random cluster token.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "hrs_" + base64.RawURLEncoding.EncodeToString(b), nil
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, id identity) error

func (s *Server) authed(adminOnly bool) func(handlerFunc) http.Handler {
	return func(h handlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || tok == "" {
				writeErr(w, http.StatusUnauthorized, api.Error{Error: "missing bearer token"})
				return
			}
			var id identity
			if len(s.admin) > 0 && subtle.ConstantTimeCompare([]byte(tok), s.admin) == 1 {
				id.admin = true
			} else {
				c, err := s.opt.Store.ClusterByTokenHash(r.Context(), HashToken(tok))
				if err != nil {
					if !errors.Is(err, store.ErrNotFound) {
						s.opt.Log.Error("token lookup failed", "err", err)
					}
					writeErr(w, http.StatusUnauthorized, api.Error{Error: "invalid token"})
					return
				}
				id.cluster = c
			}
			if adminOnly && !id.admin {
				writeErr(w, http.StatusForbidden, api.Error{Error: "admin token required"})
				return
			}
			if err := h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)), id); err != nil {
				s.writeStoreErr(w, err)
			}
		})
	}
}

// ---------------------------------------------------------------- helpers

const maxBody = 1 << 20

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: request body: %v", api.ErrInvalid, err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, e api.Error) { writeJSON(w, status, e) }

func (s *Server) writeStoreErr(w http.ResponseWriter, err error) {
	var ce *store.ConflictError
	var reloadErr *reloadError
	switch {
	case errors.As(err, &ce):
		writeErr(w, http.StatusConflict, api.Error{Error: err.Error(), Conflicts: ce.Conflicts})
	case errors.Is(err, api.ErrInvalid):
		writeErr(w, http.StatusUnprocessableEntity, api.Error{Error: err.Error()})
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, api.Error{Error: err.Error()})
	case errors.Is(err, store.ErrExists):
		writeErr(w, http.StatusConflict, api.Error{Error: err.Error()})
	case errors.As(err, &reloadErr):
		s.opt.Log.Error("snapshot reload failed after write", "err", reloadErr.err)
		writeErr(w, http.StatusInternalServerError, api.Error{Error: "write committed but DNS snapshot reload failed; check logs"})
	default:
		s.opt.Log.Error("request failed", "err", err)
		writeErr(w, http.StatusInternalServerError, api.Error{Error: "internal error"})
	}
}

type reloadError struct{ err error }

func (e *reloadError) Error() string { return "reload: " + e.err.Error() }

// write runs fn under the write lock and reloads the snapshot afterwards.
func (s *Server) write(ctx context.Context, fn func() error) error {
	return s.writeIf(ctx, func() (bool, error) { return true, fn() })
}

// writeIf is write for operations that may turn out to be no-ops: the snapshot
// is only reloaded when fn reports a change. Operator reconciles are mostly
// no-ops, so this keeps them cheap.
func (s *Server) writeIf(ctx context.Context, fn func() (changed bool, err error)) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	changed, err := fn()
	if err != nil || !changed {
		return err
	}
	if err := s.opt.Reload(ctx); err != nil {
		return &reloadError{err}
	}
	return nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			return
		}
		s.opt.Log.Info("api", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"dur", time.Since(start).Round(time.Microsecond))
	})
}

// ---------------------------------------------------------------- handlers

func (s *Server) whoami(w http.ResponseWriter, _ *http.Request, id identity) error {
	writeJSON(w, http.StatusOK, api.WhoAmI{Admin: id.admin, Cluster: id.cluster.ID, Zone: id.cluster.Zone})
	return nil
}

func (s *Server) listZones(w http.ResponseWriter, r *http.Request, _ identity) error {
	zones, err := s.opt.Store.ListZones(r.Context())
	if err != nil {
		return err
	}
	if zones == nil {
		zones = []api.Zone{}
	}
	writeJSON(w, http.StatusOK, zones)
	return nil
}

func (s *Server) createZone(w http.ResponseWriter, r *http.Request, id identity) error {
	var req api.CreateZoneRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	var z api.Zone
	err := s.write(r.Context(), func() (err error) {
		z, err = s.opt.Store.CreateZone(r.Context(), id.actor(), api.Zone{Name: req.Name, DefaultTTL: req.DefaultTTL})
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, z)
	return nil
}

func (s *Server) deleteZone(w http.ResponseWriter, r *http.Request, id identity) error {
	err := s.write(r.Context(), func() error {
		return s.opt.Store.DeleteZone(r.Context(), id.actor(), r.PathValue("zone"))
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listRecords(w http.ResponseWriter, r *http.Request, _ identity) error {
	recs, err := s.opt.Store.ListRecords(r.Context(), r.PathValue("zone"))
	if err != nil {
		return err
	}
	if recs == nil {
		recs = []api.Record{}
	}
	writeJSON(w, http.StatusOK, recs)
	return nil
}

func (s *Server) addRecord(w http.ResponseWriter, r *http.Request, id identity) error {
	var rec api.Record
	if err := decode(r, &rec); err != nil {
		return err
	}
	if rec.ID != 0 || rec.Owner != "" {
		return fmt.Errorf("%w: id and owner are set by the server", api.ErrInvalid)
	}
	rec.Zone = r.PathValue("zone")
	err := s.write(r.Context(), func() (err error) {
		rec, err = s.opt.Store.AddRecord(r.Context(), id.actor(), rec)
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, rec)
	return nil
}

func (s *Server) deleteRecord(w http.ResponseWriter, r *http.Request, id identity) error {
	rid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || rid <= 0 {
		return fmt.Errorf("%w: record id must be a positive integer", api.ErrInvalid)
	}
	if err := s.write(r.Context(), func() error {
		return s.opt.Store.DeleteRecord(r.Context(), id.actor(), rid)
	}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listClusters(w http.ResponseWriter, r *http.Request, _ identity) error {
	cs, err := s.opt.Store.ListClusters(r.Context())
	if err != nil {
		return err
	}
	if cs == nil {
		cs = []api.Cluster{}
	}
	writeJSON(w, http.StatusOK, cs)
	return nil
}

func (s *Server) createCluster(w http.ResponseWriter, r *http.Request, id identity) error {
	var req api.CreateClusterRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	tok, err := NewToken()
	if err != nil {
		return err
	}
	var c api.Cluster
	if err := s.write(r.Context(), func() (err error) {
		c, err = s.opt.Store.CreateCluster(r.Context(), id.actor(), req.ID, req.Zone, HashToken(tok))
		return err
	}); err != nil {
		return err
	}
	c.Token = tok
	writeJSON(w, http.StatusCreated, c)
	return nil
}

func (s *Server) rotateCluster(w http.ResponseWriter, r *http.Request, id identity) error {
	tok, err := NewToken()
	if err != nil {
		return err
	}
	cid := r.PathValue("id")
	if err := s.write(r.Context(), func() error {
		return s.opt.Store.RotateClusterToken(r.Context(), id.actor(), cid, HashToken(tok))
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, api.Cluster{ID: cid, Token: tok})
	return nil
}

func (s *Server) deleteCluster(w http.ResponseWriter, r *http.Request, id identity) error {
	if err := s.write(r.Context(), func() error {
		return s.opt.Store.DeleteCluster(r.Context(), id.actor(), r.PathValue("id"))
	}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) backup(w http.ResponseWriter, r *http.Request, _ identity) error {
	if s.opt.BackupDir == "" {
		return fmt.Errorf("%w: backups are disabled (HORUS_BACKUP_DIR)", api.ErrInvalid)
	}
	if err := os.MkdirAll(s.opt.BackupDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.opt.BackupDir, "horus-"+time.Now().UTC().Format("20060102T150405Z")+".db")
	if err := s.opt.Store.Backup(r.Context(), path); err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": path})
	return nil
}

// ---------------------------------------------------------------- owners (cluster tokens)

var (
	// Kubernetes namespace: DNS-1123 label.
	nsRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	// Kubernetes object name: DNS-1123 subdomain.
	objRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

func ownerOf(r *http.Request, id identity) (string, error) {
	if id.admin {
		return "", fmt.Errorf("%w: owner endpoints require a cluster token", errForbidden)
	}
	ns, name := r.PathValue("namespace"), r.PathValue("name")
	if !nsRe.MatchString(ns) || !objRe.MatchString(name) {
		return "", fmt.Errorf("%w: invalid namespace/name %q/%q", api.ErrInvalid, ns, name)
	}
	return id.cluster.ID + "/" + ns + "/" + name, nil
}

var errForbidden = errors.New("forbidden")

func (s *Server) forbiddenOr(w http.ResponseWriter, err error) error {
	if errors.Is(err, errForbidden) {
		writeErr(w, http.StatusForbidden, api.Error{Error: err.Error()})
		return nil
	}
	return err
}

func (s *Server) listOwners(w http.ResponseWriter, r *http.Request, id identity) error {
	if id.admin {
		return s.forbiddenOr(w, fmt.Errorf("%w: owner endpoints require a cluster token", errForbidden))
	}
	prefix := id.cluster.ID + "/"
	owners, err := s.opt.Store.ListOwners(r.Context(), prefix)
	if err != nil {
		return err
	}
	resp := api.OwnersResponse{Owners: make([]string, 0, len(owners))}
	for _, o := range owners {
		resp.Owners = append(resp.Owners, strings.TrimPrefix(o, prefix))
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) replaceOwner(w http.ResponseWriter, r *http.Request, id identity) error {
	owner, err := ownerOf(r, id)
	if err != nil {
		return s.forbiddenOr(w, err)
	}
	var req api.OwnerRecordsRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	for _, rec := range req.Records {
		if rec.ID != 0 || rec.Owner != "" || (rec.Zone != "" && api.NormalizeName(rec.Zone) != id.cluster.Zone) {
			return fmt.Errorf("%w: id/owner/zone are set by the server", api.ErrInvalid)
		}
	}
	var resp api.OwnerRecordsResponse
	if err := s.writeIf(r.Context(), func() (bool, error) {
		var err error
		resp, err = s.opt.Store.ReplaceOwner(r.Context(), id.actor(), id.cluster.Zone, owner, req.Records)
		return resp.Changed(), err
	}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func (s *Server) deleteOwner(w http.ResponseWriter, r *http.Request, id identity) error {
	owner, err := ownerOf(r, id)
	if err != nil {
		return s.forbiddenOr(w, err)
	}
	if err := s.writeIf(r.Context(), func() (bool, error) {
		n, err := s.opt.Store.DeleteOwner(r.Context(), id.actor(), owner)
		return n > 0, err
	}); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
