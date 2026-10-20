// Package client is the Go client for the horus REST API. It is used by the
// horus CLI and by the Kubernetes operator, and depends only on the stdlib.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Warewave-Technology/horus/pkg/api"
)

// Client talks to a horus server.
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// Options configures a Client.
type Options struct {
	// TLS is used for https:// addresses (e.g. a custom CA). Nil uses system roots.
	TLS *tls.Config
	// Timeout per request. Default 10s.
	Timeout time.Duration
}

// New creates a client. addr is either "unix:///path/to/api.sock" or an
// http(s):// base URL such as "https://horus.internal:8053".
func New(addr, token string, opt Options) (*Client, error) {
	if opt.Timeout == 0 {
		opt.Timeout = 10 * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = opt.TLS
	base := strings.TrimRight(addr, "/")
	if sock, ok := strings.CutPrefix(addr, "unix://"); ok {
		if sock == "" {
			return nil, errors.New("client: empty unix socket path")
		}
		var d net.Dialer
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", sock)
		}
		tr.Proxy = nil
		base = "http://horus"
	} else if u, err := url.Parse(addr); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("client: address %q must be unix:///path or http(s)://host:port", addr)
	}
	return &Client{base: base, token: token, hc: &http.Client{Transport: tr, Timeout: opt.Timeout}}, nil
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Body   api.Error
}

func (e *APIError) Error() string { return fmt.Sprintf("horus: %d: %s", e.Status, e.Body.Error) }

// Conflicts returns the first-wins conflicts carried by a 409, if any.
func (e *APIError) Conflicts() []api.Conflict { return e.Body.Conflicts }

// IsStatus reports whether err is an APIError with the given HTTP status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		ae := &APIError{Status: resp.StatusCode}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if json.Unmarshal(b, &ae.Body) != nil || ae.Body.Error == "" {
			ae.Body.Error = strings.TrimSpace(string(b))
		}
		return ae
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func esc(s string) string { return url.PathEscape(s) }

// WhoAmI returns the identity behind the token.
func (c *Client) WhoAmI(ctx context.Context) (api.WhoAmI, error) {
	var w api.WhoAmI
	return w, c.do(ctx, http.MethodGet, "/v1/whoami", nil, &w)
}

// ---- zones / records (admin)

func (c *Client) ListZones(ctx context.Context) ([]api.Zone, error) {
	var z []api.Zone
	return z, c.do(ctx, http.MethodGet, "/v1/zones", nil, &z)
}

func (c *Client) CreateZone(ctx context.Context, name string, defaultTTL uint32) (api.Zone, error) {
	var z api.Zone
	return z, c.do(ctx, http.MethodPost, "/v1/zones", api.CreateZoneRequest{Name: name, DefaultTTL: defaultTTL}, &z)
}

func (c *Client) DeleteZone(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/zones/"+esc(name), nil, nil)
}

func (c *Client) ListRecords(ctx context.Context, zone string) ([]api.Record, error) {
	var r []api.Record
	return r, c.do(ctx, http.MethodGet, "/v1/zones/"+esc(zone)+"/records", nil, &r)
}

func (c *Client) AddRecord(ctx context.Context, zone string, r api.Record) (api.Record, error) {
	var out api.Record
	return out, c.do(ctx, http.MethodPost, "/v1/zones/"+esc(zone)+"/records", r, &out)
}

func (c *Client) DeleteRecord(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, fmt.Sprintf("/v1/records/%d", id), nil, nil)
}

// ---- clusters (admin)

func (c *Client) ListClusters(ctx context.Context) ([]api.Cluster, error) {
	var cs []api.Cluster
	return cs, c.do(ctx, http.MethodGet, "/v1/clusters", nil, &cs)
}

// CreateCluster returns the cluster including its token (shown only once).
func (c *Client) CreateCluster(ctx context.Context, id string, zones []string) (api.Cluster, error) {
	var out api.Cluster
	return out, c.do(ctx, http.MethodPost, "/v1/clusters", api.CreateClusterRequest{ID: id, Zones: zones}, &out)
}

// AddClusterZone gives the cluster one more zone and returns the cluster.
func (c *Client) AddClusterZone(ctx context.Context, id, zone string) (api.Cluster, error) {
	var out api.Cluster
	return out, c.do(ctx, http.MethodPost, "/v1/clusters/"+esc(id)+"/zones", api.ClusterZoneRequest{Zone: zone}, &out)
}

// RemoveClusterZone takes a zone away from the cluster, deleting the cluster's
// records in it. It returns how many records were deleted.
func (c *Client) RemoveClusterZone(ctx context.Context, id, zone string) (int, error) {
	var out api.RemoveClusterZoneResponse
	err := c.do(ctx, http.MethodDelete, "/v1/clusters/"+esc(id)+"/zones/"+esc(zone), nil, &out)
	return out.RecordsRemoved, err
}

// RotateClusterToken returns the new token.
func (c *Client) RotateClusterToken(ctx context.Context, id string) (string, error) {
	var out api.Cluster
	err := c.do(ctx, http.MethodPost, "/v1/clusters/"+esc(id)+"/rotate-token", nil, &out)
	return out.Token, err
}

func (c *Client) DeleteCluster(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/clusters/"+esc(id), nil, nil)
}

// Backup asks the server to write a consistent DB copy; returns its path on the server.
func (c *Client) Backup(ctx context.Context) (string, error) {
	var out map[string]string
	err := c.do(ctx, http.MethodPost, "/v1/backup", nil, &out)
	return out["path"], err
}

// ---- owners (cluster token)

// ListOwners returns the calling cluster's owners as "namespace/name".
func (c *Client) ListOwners(ctx context.Context) ([]string, error) {
	var out api.OwnersResponse
	return out.Owners, c.do(ctx, http.MethodGet, "/v1/owners", nil, &out)
}

// ReplaceOwner sets the complete record set of namespace/name.
func (c *Client) ReplaceOwner(ctx context.Context, namespace, name string, recs []api.Record) (api.OwnerRecordsResponse, error) {
	if recs == nil {
		recs = []api.Record{}
	}
	var out api.OwnerRecordsResponse
	return out, c.do(ctx, http.MethodPut, "/v1/owners/"+esc(namespace)+"/"+esc(name)+"/records",
		api.OwnerRecordsRequest{Records: recs}, &out)
}

// DeleteOwner removes every record of namespace/name. It is idempotent.
func (c *Client) DeleteOwner(ctx context.Context, namespace, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/owners/"+esc(namespace)+"/"+esc(name), nil, nil)
}
