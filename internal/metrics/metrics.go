// Package metrics is a tiny Prometheus text-exposition registry: counters with
// a fixed label set and gauge callbacks. It avoids pulling client_golang (and
// its dependency tree) into a deliberately small binary. If richer metrics are
// ever needed, swapping this for client_golang is a local change.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Counter is a monotonically increasing counter.
type Counter struct{ v atomic.Uint64 }

// Inc adds one.
func (c *Counter) Inc() { c.v.Add(1) }

// Add adds n.
func (c *Counter) Add(n uint64) { c.v.Add(n) }

// Value returns the current value.
func (c *Counter) Value() uint64 { return c.v.Load() }

type family struct {
	name, help, kind string
	label            string              // single label name for counter vecs, "" otherwise
	counters         map[string]*Counter // label value -> counter
	gauge            func() float64      // for gauges
	mu               sync.Mutex
}

// Registry holds metric families.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{families: map[string]*family{}} }

// CounterVec is a counter family with one label.
type CounterVec struct{ f *family }

// With returns the counter for a label value, creating it on first use.
// On a zero CounterVec (metrics disabled) it returns a throwaway counter.
func (v CounterVec) With(value string) *Counter {
	if v.f == nil {
		return &Counter{}
	}
	v.f.mu.Lock()
	defer v.f.mu.Unlock()
	c, ok := v.f.counters[value]
	if !ok {
		c = &Counter{}
		v.f.counters[value] = c
	}
	return c
}

// NewCounterVec registers a counter family with a single label.
func (r *Registry) NewCounterVec(name, help, label string) CounterVec {
	f := &family{name: name, help: help, kind: "counter", label: label, counters: map[string]*Counter{}}
	r.add(f)
	return CounterVec{f}
}

// NewCounter registers a counter without labels.
func (r *Registry) NewCounter(name, help string) *Counter {
	f := &family{name: name, help: help, kind: "counter", counters: map[string]*Counter{}}
	r.add(f)
	c := &Counter{}
	f.counters[""] = c
	return c
}

// NewGaugeFunc registers a gauge whose value is read at scrape time.
func (r *Registry) NewGaugeFunc(name, help string, fn func() float64) {
	r.add(&family{name: name, help: help, kind: "gauge", gauge: fn})
}

func (r *Registry) add(f *family) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.families[f.name]; dup {
		panic("metrics: duplicate family " + f.name)
	}
	r.families[f.name] = f
}

// WriteTo writes the text exposition format.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	r.mu.Unlock()
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		r.mu.Lock()
		f := r.families[n]
		r.mu.Unlock()
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.kind)
		if f.kind == "gauge" {
			fmt.Fprintf(&b, "%s %g\n", f.name, f.gauge())
			continue
		}
		f.mu.Lock()
		vals := make([]string, 0, len(f.counters))
		for v := range f.counters {
			vals = append(vals, v)
		}
		sort.Strings(vals)
		for _, v := range vals {
			if f.label == "" {
				fmt.Fprintf(&b, "%s %d\n", f.name, f.counters[v].Value())
			} else {
				fmt.Fprintf(&b, "%s{%s=%q} %d\n", f.name, f.label, v, f.counters[v].Value())
			}
		}
		f.mu.Unlock()
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

// Handler serves the registry over HTTP.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = r.WriteTo(w)
	})
}
