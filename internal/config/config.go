// Package config loads the server configuration from HORUS_* environment
// variables. Under systemd these come from EnvironmentFile=/etc/horus/horus.env,
// which keeps the binary dependency-free and the unit file the single source of
// deployment truth.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is the server configuration.
type Config struct {
	DNSListen string // HORUS_DNS_LISTEN, UDP+TCP, default ":53"

	APIListen  string // HORUS_API_LISTEN, TCP for operators, e.g. ":8053"; empty disables
	APISocket  string // HORUS_API_SOCKET, unix socket for the CLI; "none" disables
	TLSCert    string // HORUS_API_TLS_CERT, or $CREDENTIALS_DIRECTORY/api-tls-cert
	TLSKey     string // HORUS_API_TLS_KEY, or $CREDENTIALS_DIRECTORY/api-tls-key
	AdminToken string // read from HORUS_ADMIN_TOKEN_FILE or $CREDENTIALS_DIRECTORY/admin-token

	DBPath    string // HORUS_DB
	BackupDir string // HORUS_BACKUP_DIR

	Upstreams       []string      // HORUS_UPSTREAMS, comma separated host:port
	UpstreamTimeout time.Duration // HORUS_UPSTREAM_TIMEOUT

	CacheMaxEntries int           // HORUS_CACHE_MAX_ENTRIES
	CacheMaxTTL     time.Duration // HORUS_CACHE_MAX_TTL
	CacheNegTTL     time.Duration // HORUS_CACHE_NEG_TTL (cap for NXDOMAIN/NODATA)

	SOAMName string // HORUS_SOA_MNAME, primary NS name in synthesized SOA
	SOARName string // HORUS_SOA_RNAME, hostmaster mailbox in synthesized SOA

	LogLevel string // HORUS_LOG_LEVEL: debug|info|warn|error
	QueryLog bool   // HORUS_QUERY_LOG: log every query at info level
}

// Defaults returns the built-in defaults.
func Defaults() Config {
	return Config{
		DNSListen:       ":53",
		APISocket:       "/run/horus/api.sock",
		DBPath:          "/var/lib/horus/horus.db",
		BackupDir:       "/var/lib/horus/backups",
		Upstreams:       []string{"1.1.1.1:53", "9.9.9.9:53"},
		UpstreamTimeout: 2 * time.Second,
		CacheMaxEntries: 10000,
		CacheMaxTTL:     time.Hour,
		CacheNegTTL:     5 * time.Minute,
		SOAMName:        "ns.horus.",
		SOARName:        "hostmaster.horus.",
		LogLevel:        "info",
	}
}

// FromEnv loads the configuration using getenv (os.Getenv in production).
func FromEnv(getenv func(string) string) (Config, error) {
	c := Defaults()
	str := func(key string, dst *string) {
		if v, ok := lookup(getenv, key); ok {
			*dst = v
		}
	}
	var errs []string
	dur := func(key string, dst *time.Duration) {
		if v, ok := lookup(getenv, key); ok {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				errs = append(errs, fmt.Sprintf("%s=%q: must be a positive duration", key, v))
				return
			}
			*dst = d
		}
	}

	str("HORUS_DNS_LISTEN", &c.DNSListen)
	str("HORUS_API_LISTEN", &c.APIListen)
	str("HORUS_API_SOCKET", &c.APISocket)
	str("HORUS_API_TLS_CERT", &c.TLSCert)
	str("HORUS_API_TLS_KEY", &c.TLSKey)
	str("HORUS_DB", &c.DBPath)
	str("HORUS_BACKUP_DIR", &c.BackupDir)
	str("HORUS_SOA_MNAME", &c.SOAMName)
	str("HORUS_SOA_RNAME", &c.SOARName)
	str("HORUS_LOG_LEVEL", &c.LogLevel)
	if c.APISocket == "none" { // explicit opt-out of the default socket
		c.APISocket = ""
	}
	dur("HORUS_UPSTREAM_TIMEOUT", &c.UpstreamTimeout)
	dur("HORUS_CACHE_MAX_TTL", &c.CacheMaxTTL)
	dur("HORUS_CACHE_NEG_TTL", &c.CacheNegTTL)

	if v, ok := lookup(getenv, "HORUS_UPSTREAMS"); ok {
		c.Upstreams = nil
		for _, u := range strings.Split(v, ",") {
			if u = strings.TrimSpace(u); u == "" {
				continue
			}
			if _, _, err := net.SplitHostPort(u); err != nil {
				u = net.JoinHostPort(u, "53")
			}
			c.Upstreams = append(c.Upstreams, u)
		}
	}
	if v, ok := lookup(getenv, "HORUS_CACHE_MAX_ENTRIES"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Sprintf("HORUS_CACHE_MAX_ENTRIES=%q: must be a positive integer", v))
		}
		c.CacheMaxEntries = n
	}
	if v, ok := lookup(getenv, "HORUS_QUERY_LOG"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Sprintf("HORUS_QUERY_LOG=%q: must be a boolean", v))
		}
		c.QueryLog = b
	}

	// Admin token: explicit file, else systemd credential.
	tokenFile := getenv("HORUS_ADMIN_TOKEN_FILE")
	if tokenFile == "" {
		if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" {
			tokenFile = filepath.Join(dir, "admin-token")
		}
	}
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			errs = append(errs, fmt.Sprintf("admin token: %v", err))
		}
		c.AdminToken = strings.TrimSpace(string(b))
	}

	// API TLS: explicit paths, else systemd credentials (LoadCredential=
	// api-tls-cert / api-tls-key), which keeps the key readable under
	// DynamicUser=yes without loosening its file permissions.
	if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" && c.TLSCert == "" && c.TLSKey == "" {
		for _, cred := range []struct {
			name string
			dst  *string
		}{{"api-tls-cert", &c.TLSCert}, {"api-tls-key", &c.TLSKey}} {
			p := filepath.Join(dir, cred.name)
			if _, err := os.Stat(p); err == nil {
				*cred.dst = p
			}
		}
	}

	if len(errs) > 0 {
		return c, fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return c, c.Validate()
}

func lookup(getenv func(string) string, key string) (string, bool) {
	v := strings.TrimSpace(getenv(key))
	return v, v != ""
}

// Validate checks cross-field rules.
func (c Config) Validate() error {
	var errs []string
	if c.DNSListen == "" {
		errs = append(errs, "HORUS_DNS_LISTEN must not be empty")
	}
	if c.APIListen == "" && c.APISocket == "" {
		errs = append(errs, "at least one of HORUS_API_LISTEN or HORUS_API_SOCKET is required")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		errs = append(errs, "HORUS_API_TLS_CERT and HORUS_API_TLS_KEY (or credentials api-tls-cert and api-tls-key) must be set together")
	}
	if len(c.AdminToken) < 32 {
		errs = append(errs, "admin token missing or shorter than 32 characters (HORUS_ADMIN_TOKEN_FILE or systemd credential admin-token)")
	}
	if len(c.Upstreams) == 0 {
		errs = append(errs, "HORUS_UPSTREAMS must list at least one upstream")
	}
	if c.DBPath == "" {
		errs = append(errs, "HORUS_DB must not be empty")
	}
	for _, n := range []struct{ k, v string }{{"HORUS_SOA_MNAME", c.SOAMName}, {"HORUS_SOA_RNAME", c.SOARName}} {
		if !strings.HasSuffix(n.v, ".") {
			errs = append(errs, n.k+" must be a FQDN with trailing dot")
		}
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, "HORUS_LOG_LEVEL must be debug|info|warn|error")
	}
	if len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return nil
}
