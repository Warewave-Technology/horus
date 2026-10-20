package dnsserver

import (
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/warewave/horus/internal/cache"
	"github.com/warewave/horus/internal/metrics"
)

// Forwarder resolves non-authoritative queries via upstream resolvers, with a
// TTL-respecting cache (including RFC 2308 negative caching) and singleflight
// deduplication of identical in-flight queries.
type Forwarder struct {
	upstreams []string
	udp, tcp  *dns.Client
	cache     *cache.Cache
	sf        cache.Group[*dns.Msg]
	maxTTL    time.Duration
	negTTL    time.Duration
	preferred atomic.Int32 // index of the last upstream that answered
	log       *slog.Logger

	upstreamErrors metrics.CounterVec
}

// ForwarderOptions configures a Forwarder.
type ForwarderOptions struct {
	Upstreams []string // host:port, tried in order starting from the last good one
	Timeout   time.Duration
	CacheSize int
	MaxTTL    time.Duration // cap for positive answers
	NegTTL    time.Duration // cap for NXDOMAIN/NODATA
	Log       *slog.Logger
	Metrics   *metrics.Registry
}

// NewForwarder returns a Forwarder.
func NewForwarder(o ForwarderOptions) *Forwarder {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	f := &Forwarder{
		upstreams: o.Upstreams,
		udp:       &dns.Client{Net: "udp", Timeout: o.Timeout, UDPSize: ednsSize},
		tcp:       &dns.Client{Net: "tcp", Timeout: o.Timeout},
		cache:     cache.New(o.CacheSize),
		maxTTL:    o.MaxTTL,
		negTTL:    o.NegTTL,
		log:       o.Log,
	}
	if o.Metrics != nil {
		f.upstreamErrors = o.Metrics.NewCounterVec("horus_upstream_errors_total",
			"Failed upstream exchanges by upstream.", "upstream")
		o.Metrics.NewGaugeFunc("horus_cache_entries", "Entries in the forwarding cache.", func() float64 {
			n, _, _ := f.cache.Stats()
			return float64(n)
		})
	}
	return f
}

// Cache exposes the cache (for sweeping and flushing).
func (f *Forwarder) Cache() *cache.Cache { return f.cache }

func cacheKey(q dns.Question) string {
	return strings.ToLower(q.Name) + "|" + strconv.Itoa(int(q.Qtype)) + "|" + strconv.Itoa(int(q.Qclass))
}

// Resolve answers q from cache or upstream. The returned message is owned by
// the caller. cached reports whether it came from the cache.
func (f *Forwarder) Resolve(q dns.Question) (m *dns.Msg, cached bool, err error) {
	key := cacheKey(q)
	if e, ok := f.cache.Get(key); ok {
		m = new(dns.Msg)
		if err := m.Unpack(e.Msg); err == nil {
			age := uint32(time.Since(e.Stored) / time.Second)
			decrementTTL(m, age)
			return m, true, nil
		}
	}
	shared, err, _ := f.sf.Do(key, func() (*dns.Msg, error) { return f.exchange(key, q) })
	if err != nil {
		return nil, false, err
	}
	return shared.Copy(), false, nil
}

func (f *Forwarder) exchange(key string, q dns.Question) (*dns.Msg, error) {
	req := new(dns.Msg)
	req.SetQuestion(q.Name, q.Qtype)
	req.Question[0].Qclass = q.Qclass
	req.RecursionDesired = true
	req.SetEdns0(ednsSize, false)

	n := len(f.upstreams)
	if n == 0 {
		return nil, fmt.Errorf("no upstreams configured")
	}
	start := int(f.preferred.Load())
	var lastErr error
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		u := f.upstreams[idx]
		resp, _, err := f.udp.Exchange(req, u)
		if err == nil && resp.Truncated {
			resp, _, err = f.tcp.Exchange(req, u)
		}
		if err == nil && resp.Rcode != dns.RcodeSuccess && resp.Rcode != dns.RcodeNameError {
			err = fmt.Errorf("upstream %s answered %s", u, dns.RcodeToString[resp.Rcode])
		}
		if err != nil {
			lastErr = err
			f.upstreamErrors.With(u).Inc()
			f.log.Debug("upstream failed", "upstream", u, "qname", q.Name, "err", err)
			continue
		}
		f.preferred.Store(int32(idx))
		if ttl, ok := f.cacheTTL(resp); ok {
			if b, err := resp.Pack(); err == nil {
				f.cache.Set(key, b, ttl)
			}
		}
		return resp, nil
	}
	return nil, lastErr
}

// cacheTTL decides how long a response may be cached.
func (f *Forwarder) cacheTTL(m *dns.Msg) (time.Duration, bool) {
	if m.Truncated {
		return 0, false
	}
	if m.Rcode == dns.RcodeNameError || len(m.Answer) == 0 {
		// RFC 2308: negative answers are cacheable only with an SOA in the
		// authority section, for min(SOA TTL, SOA MINIMUM).
		for _, rr := range m.Ns {
			if soa, ok := rr.(*dns.SOA); ok {
				d := time.Duration(min(soa.Hdr.Ttl, soa.Minttl)) * time.Second
				return min(d, f.negTTL), d > 0
			}
		}
		return 0, false
	}
	lowest := uint32(math.MaxUint32)
	for _, sec := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range sec {
			if rr.Header().Rrtype == dns.TypeOPT {
				continue
			}
			lowest = min(lowest, rr.Header().Ttl)
		}
	}
	d := time.Duration(lowest) * time.Second
	return min(d, f.maxTTL), d > 0
}

func decrementTTL(m *dns.Msg, age uint32) {
	for _, sec := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range sec {
			h := rr.Header()
			if h.Rrtype == dns.TypeOPT {
				continue
			}
			if h.Ttl > age {
				h.Ttl -= age
			} else {
				h.Ttl = 0
			}
		}
	}
}
