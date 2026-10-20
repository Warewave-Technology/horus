package metrics

import (
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	r := NewRegistry()
	q := r.NewCounterVec("horus_dns_queries_total", "DNS queries by result.", "result")
	q.With("authoritative").Add(3)
	q.With("forwarded").Inc()
	e := r.NewCounter("horus_upstream_errors_total", "Upstream exchange errors.")
	e.Inc()
	r.NewGaugeFunc("horus_records", "Records in the snapshot.", func() float64 { return 7 })

	var b strings.Builder
	if _, err := r.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		`horus_dns_queries_total{result="authoritative"} 3`,
		`horus_dns_queries_total{result="forwarded"} 1`,
		"horus_upstream_errors_total 1",
		"horus_records 7",
		"# TYPE horus_records gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
