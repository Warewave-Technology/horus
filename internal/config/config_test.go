package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func tokenFile(t *testing.T, tok string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "admin-token")
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFromEnvDefaults(t *testing.T) {
	tok := strings.Repeat("a", 40)
	c, err := FromEnv(env(map[string]string{"HORUS_ADMIN_TOKEN_FILE": tokenFile(t, tok)}))
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminToken != tok || c.DNSListen != ":53" || c.APISocket == "" || len(c.Upstreams) != 2 {
		t.Fatalf("%+v", c)
	}
}

func TestFromEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "admin-token"), []byte(strings.Repeat("b", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := FromEnv(env(map[string]string{
		"CREDENTIALS_DIRECTORY":  dir,
		"HORUS_UPSTREAMS":        "10.0.0.53, 10.0.0.54:5353 ,",
		"HORUS_UPSTREAM_TIMEOUT": "500ms",
		"HORUS_API_LISTEN":       ":8053",
		"HORUS_QUERY_LOG":        "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Upstreams, ","); got != "10.0.0.53:53,10.0.0.54:5353" {
		t.Fatalf("upstreams: %s", got)
	}
	if c.UpstreamTimeout != 500*time.Millisecond || c.APIListen != ":8053" || !c.QueryLog {
		t.Fatalf("%+v", c)
	}
}

func TestFromEnvErrors(t *testing.T) {
	good := tokenFile(t, strings.Repeat("a", 40))
	for name, m := range map[string]map[string]string{
		"no token":      {},
		"short token":   {"HORUS_ADMIN_TOKEN_FILE": tokenFile(t, "short")},
		"bad duration":  {"HORUS_ADMIN_TOKEN_FILE": good, "HORUS_CACHE_MAX_TTL": "soon"},
		"half tls":      {"HORUS_ADMIN_TOKEN_FILE": good, "HORUS_API_TLS_CERT": "/x.crt"},
		"bad log level": {"HORUS_ADMIN_TOKEN_FILE": good, "HORUS_LOG_LEVEL": "loud"},
		"no api":        {"HORUS_ADMIN_TOKEN_FILE": good, "HORUS_API_SOCKET": "none"},
	} {
		if _, err := FromEnv(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
