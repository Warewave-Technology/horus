// Package dnsserver is the DNS front end: it answers from the in-memory zone
// snapshot when authoritative and forwards everything else upstream.
package dnsserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/miekg/dns"

	"github.com/Warewave-Technology/horus/internal/metrics"
	"github.com/Warewave-Technology/horus/internal/zone"
	"github.com/Warewave-Technology/horus/pkg/api"
)

// ednsSize is the EDNS0 UDP payload size we advertise and request
// (DNS Flag Day 2020 recommendation, avoids IP fragmentation).
const ednsSize = 1232

// negativeTTLCap bounds the SOA MINIMUM we hand out for our own zones. It is
// deliberately short: a freshly registered Ingress should become resolvable
// quickly for clients that already cached its NXDOMAIN.
const negativeTTLCap = 30

// Options configures the Handler.
type Options struct {
	Snapshots *zone.Holder
	Forwarder *Forwarder
	SOAMName  string // e.g. "ns.horus."
	SOARName  string // e.g. "hostmaster.horus."
	Log       *slog.Logger
	QueryLog  bool
	Metrics   *metrics.Registry
}

// Handler implements dns.Handler.
type Handler struct {
	opt     Options
	queries metrics.CounterVec
}

// NewHandler returns a Handler.
func NewHandler(o Options) *Handler {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	h := &Handler{opt: o}
	if o.Metrics != nil {
		h.queries = o.Metrics.NewCounterVec("horus_dns_queries_total", "DNS queries by result.", "result")
	}
	return h
}

// ServeDNS answers one query.
func (h *Handler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	start := time.Now()
	_, udp := w.RemoteAddr().(*net.UDPAddr)

	m, result := h.answer(r)
	h.finalize(r, m, udp)

	if err := w.WriteMsg(m); err != nil {
		h.opt.Log.Debug("write failed", "err", err)
	}
	h.queries.With(result).Inc()
	if h.opt.QueryLog && len(r.Question) == 1 {
		q := r.Question[0]
		h.opt.Log.Info("query",
			"client", w.RemoteAddr().String(),
			"qname", q.Name,
			"qtype", dns.TypeToString[q.Qtype],
			"rcode", dns.RcodeToString[m.Rcode],
			"result", result,
			"dur", time.Since(start).Round(time.Microsecond))
	}
}

func (h *Handler) answer(r *dns.Msg) (*dns.Msg, string) {
	if r.Opcode != dns.OpcodeQuery {
		return errorReply(r, dns.RcodeNotImplemented), "notimp"
	}
	if len(r.Question) != 1 {
		return errorReply(r, dns.RcodeFormatError), "formerr"
	}
	if opt := r.IsEdns0(); opt != nil && opt.Version() != 0 {
		// finalize adds the OPT record that carries the extended rcode.
		return errorReply(r, dns.RcodeBadVers), "badvers"
	}
	q := r.Question[0]
	if q.Qclass != dns.ClassINET {
		return errorReply(r, dns.RcodeRefused), "refused"
	}

	res := h.opt.Snapshots.Load().Lookup(q.Name, q.Qtype)
	if !res.Authoritative {
		return h.forward(r, q)
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	m.RecursionAvailable = true
	for _, rec := range res.Answer {
		if rr := toRR(rec); rr != nil {
			m.Answer = append(m.Answer, rr)
		}
	}
	if res.SOA {
		m.Answer = append(m.Answer, h.soa(res.Zone))
	}
	if res.ChaseTarget != "" {
		// Our CNAME points outside our zones: complete the chain upstream so
		// stub resolvers get a usable answer. On failure the client still
		// gets the CNAME and may chase it itself.
		up, _, err := h.opt.Forwarder.Resolve(dns.Question{Name: res.ChaseTarget, Qtype: q.Qtype, Qclass: dns.ClassINET})
		if err == nil {
			m.Answer = append(m.Answer, up.Answer...)
			if up.Rcode == dns.RcodeNameError {
				m.Rcode = dns.RcodeNameError
			}
			if up.Rcode == dns.RcodeNameError || len(up.Answer) == 0 {
				// Negative upstream answer: pass its SOA on for negative caching.
				m.Ns = append(m.Ns, up.Ns...)
			}
		}
		return m, "authoritative"
	}
	switch {
	case res.Rcode == zone.NXDomain:
		m.Rcode = dns.RcodeNameError
		m.Ns = append(m.Ns, h.soa(res.Zone))
		return m, "nxdomain"
	case res.NoData:
		m.Ns = append(m.Ns, h.soa(res.Zone))
		return m, "nodata"
	}
	return m, "authoritative"
}

func (h *Handler) forward(r *dns.Msg, q dns.Question) (*dns.Msg, string) {
	up, cached, err := h.opt.Forwarder.Resolve(q)
	if err != nil {
		h.opt.Log.Warn("forward failed", "qname", q.Name, "qtype", dns.TypeToString[q.Qtype], "err", err)
		return errorReply(r, dns.RcodeServerFailure), "servfail"
	}
	m := up
	m.Id = r.Id
	m.Response = true
	m.Opcode = r.Opcode
	m.RecursionDesired = r.RecursionDesired
	m.RecursionAvailable = true
	m.Authoritative = false
	m.Question = r.Question // preserve the client's casing (0x20)
	if cached {
		return m, "cache_hit"
	}
	return m, "forwarded"
}

// finalize applies EDNS0 and UDP truncation to the outgoing message.
func (h *Handler) finalize(r, m *dns.Msg, udp bool) {
	// Drop any OPT that came from upstream; ours is added below.
	extra := m.Extra[:0]
	for _, rr := range m.Extra {
		if rr.Header().Rrtype != dns.TypeOPT {
			extra = append(extra, rr)
		}
	}
	m.Extra = extra

	size := dns.MinMsgSize
	if opt := r.IsEdns0(); opt != nil {
		m.SetEdns0(ednsSize, opt.Do())
		size = max(int(opt.UDPSize()), dns.MinMsgSize)
		size = min(size, ednsSize)
	}
	if udp {
		m.Truncate(size)
	}
	m.Compress = true
}

func errorReply(r *dns.Msg, rcode int) *dns.Msg {
	m := new(dns.Msg)
	m.SetRcode(r, rcode)
	m.RecursionAvailable = true
	return m
}

func (h *Handler) soa(z *api.Zone) dns.RR {
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: z.Name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: z.DefaultTTL},
		Ns:      h.opt.SOAMName,
		Mbox:    h.opt.SOARName,
		Serial:  z.Serial,
		Refresh: 3600,
		Retry:   600,
		Expire:  86400,
		Minttl:  min(z.DefaultTTL, negativeTTLCap),
	}
}

func toRR(r api.Record) dns.RR {
	hdr := dns.RR_Header{Name: r.Name, Class: dns.ClassINET, Ttl: r.TTL}
	switch r.Type {
	case api.TypeA:
		ip := net.ParseIP(r.Data).To4()
		if ip == nil {
			return nil
		}
		hdr.Rrtype = dns.TypeA
		return &dns.A{Hdr: hdr, A: ip}
	case api.TypeAAAA:
		ip := net.ParseIP(r.Data)
		if ip == nil {
			return nil
		}
		hdr.Rrtype = dns.TypeAAAA
		return &dns.AAAA{Hdr: hdr, AAAA: ip}
	case api.TypeCNAME:
		hdr.Rrtype = dns.TypeCNAME
		return &dns.CNAME{Hdr: hdr, Target: r.Data}
	}
	return nil
}

// Listener is a running UDP+TCP DNS server pair.
type Listener struct {
	udp, tcp *dns.Server
	errc     chan error
}

// Listen binds UDP and TCP on addr and starts serving. When Listen returns
// without error both sockets are bound, so the caller can signal readiness.
func Listen(addr string, h dns.Handler) (*Listener, error) {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		pc.Close()
		return nil, err
	}
	ln := &Listener{
		udp:  &dns.Server{PacketConn: pc, Handler: h, UDPSize: dns.DefaultMsgSize},
		tcp:  &dns.Server{Listener: l, Handler: h, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second},
		errc: make(chan error, 2),
	}
	go func() { ln.errc <- ln.udp.ActivateAndServe() }()
	go func() { ln.errc <- ln.tcp.ActivateAndServe() }()
	return ln, nil
}

// Err delivers the first serve error (nil after a clean shutdown).
func (l *Listener) Err() <-chan error { return l.errc }

// Shutdown stops both servers.
func (l *Listener) Shutdown(ctx context.Context) error {
	return errors.Join(l.udp.ShutdownContext(ctx), l.tcp.ShutdownContext(ctx))
}
