package dnsserver

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/Warewave-Technology/horus/internal/zone"
	"github.com/Warewave-Technology/horus/pkg/api"
)

// fakeUpstream answers A queries for ext.example.com., lb.cloud.example.com.
// and pub.prod.internal.example. (a public name inside our zone), and NXDOMAIN
// (with an SOA whose TTL is 600) for everything else. It counts queries.
func fakeUpstream(t *testing.T) (addr string, queries *atomic.Int32) {
	t.Helper()
	queries = &atomic.Int32{}
	h := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		queries.Add(1)
		m := new(dns.Msg)
		m.SetReply(r)
		q := r.Question[0]
		switch q.Name {
		case "ext.example.com.", "lb.cloud.example.com.", "pub.prod.internal.example.":
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120},
				A:   net.ParseIP("192.0.2.10").To4(),
			})
		default:
			m.Rcode = dns.RcodeNameError
			m.Ns = append(m.Ns, &dns.SOA{
				Hdr: dns.RR_Header{Name: "example.com.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 600},
				Ns:  "ns.example.com.", Mbox: "h.example.com.", Serial: 1, Minttl: 900,
			})
		}
		w.WriteMsg(m)
	})
	ln, err := Listen("127.0.0.1:0", h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Shutdown(t.Context()) })
	return ln.udp.PacketConn.LocalAddr().String(), queries
}

func startHorus(t *testing.T, upstream string) string {
	t.Helper()
	holder := zone.NewHolder()
	holder.Store(zone.Build(
		[]api.Zone{{Name: "prod.internal.example.", DefaultTTL: 300, Serial: 7}},
		[]api.Record{
			{Zone: "prod.internal.example.", Name: "app.prod.internal.example.", Type: api.TypeA, Data: "10.0.0.1"},
			{Zone: "prod.internal.example.", Name: "ext.prod.internal.example.", Type: api.TypeCNAME, Data: "lb.cloud.example.com."},
			{Zone: "prod.internal.example.", Name: "www.prod.internal.example.", Type: api.TypeCNAME, Data: "app.prod.internal.example."},
			{Zone: "prod.internal.example.", Name: "gone.prod.internal.example.", Type: api.TypeCNAME, Data: "nope.example.com."},
			{Zone: "prod.internal.example.", Name: "topub.prod.internal.example.", Type: api.TypeCNAME, Data: "pub.prod.internal.example."},
		},
	))
	fwd := NewForwarder(ForwarderOptions{
		Upstreams: []string{upstream}, Timeout: time.Second, CacheSize: 100,
		MaxTTL: time.Hour, NegTTL: time.Minute,
	})
	h := NewHandler(Options{Snapshots: holder, Forwarder: fwd, SOAMName: "ns.horus.", SOARName: "hostmaster.horus."})
	ln, err := Listen("127.0.0.1:0", h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Shutdown(t.Context()) })
	return ln.udp.PacketConn.LocalAddr().String()
}

func query(t *testing.T, addr, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	m.SetEdns0(1232, false)
	c := &dns.Client{Timeout: 2 * time.Second}
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
	return resp
}

func TestAuthoritative(t *testing.T) {
	up, n := fakeUpstream(t)
	addr := startHorus(t, up)

	r := query(t, addr, "app.prod.internal.example.", dns.TypeA)
	if !r.Authoritative || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
		t.Fatalf("got %v", r)
	}
	if a := r.Answer[0].(*dns.A); a.A.String() != "10.0.0.1" || a.Hdr.Ttl != 300 {
		t.Fatalf("answer %v", a)
	}

	// A name we hold but without that type: NODATA with our SOA, no upstream.
	r = query(t, addr, "app.prod.internal.example.", dns.TypeAAAA)
	if !r.Authoritative || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 || len(r.Ns) != 1 {
		t.Fatalf("nodata: %v", r)
	}
	if soa := r.Ns[0].(*dns.SOA); soa.Serial != 7 || soa.Minttl != negativeTTLCap {
		t.Fatalf("soa: %v", soa)
	}

	// NODATA at the end of an in-zone CNAME chain still carries the SOA.
	r = query(t, addr, "www.prod.internal.example.", dns.TypeAAAA)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || len(r.Ns) != 1 {
		t.Fatalf("cname nodata: %v", r)
	}
	if _, ok := r.Ns[0].(*dns.SOA); !ok {
		t.Fatalf("cname nodata authority: %v", r.Ns)
	}
	if got := n.Load(); got != 0 {
		t.Fatalf("names we hold must never be forwarded, upstream saw %d queries", got)
	}
}

func TestUnknownNamesInOurZonesAreForwarded(t *testing.T) {
	up, n := fakeUpstream(t)
	addr := startHorus(t, up)

	// A public name inside our zone.
	r := query(t, addr, "pub.prod.internal.example.", dns.TypeA)
	if r.Authoritative || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
		t.Fatalf("public name in our zone: %v", r)
	}
	if a := r.Answer[0].(*dns.A); a.A.String() != "192.0.2.10" {
		t.Fatalf("answer %v", a)
	}

	// A name nobody has: upstream's NXDOMAIN, with the SOA TTL capped so a
	// name registered later becomes visible within negativeTTLCap.
	for i := 0; i < 2; i++ { // second answer comes from the cache
		r = query(t, addr, "missing.prod.internal.example.", dns.TypeA)
		if r.Rcode != dns.RcodeNameError || len(r.Ns) != 1 {
			t.Fatalf("nxdomain: %v", r)
		}
		if soa := r.Ns[0].(*dns.SOA); soa.Hdr.Ttl > negativeTTLCap || soa.Minttl != 900 {
			t.Fatalf("negative TTL must be capped via the SOA TTL only: %v", soa)
		}
	}
	// The zone apex has no records here: forwarded too.
	query(t, addr, "prod.internal.example.", dns.TypeSOA)
	if got := n.Load(); got != 3 {
		t.Fatalf("expected 3 upstream queries, got %d", got)
	}

	// Outside our zones the upstream TTLs are left alone.
	r = query(t, addr, "nope.example.com.", dns.TypeA)
	if soa := r.Ns[0].(*dns.SOA); soa.Hdr.Ttl != 600 {
		t.Fatalf("SOA TTL outside our zones must not be touched: %v", soa)
	}
}

func TestCNAMEChaseUpstream(t *testing.T) {
	up, _ := fakeUpstream(t)
	addr := startHorus(t, up)
	r := query(t, addr, "ext.prod.internal.example.", dns.TypeA)
	if len(r.Answer) != 2 {
		t.Fatalf("expected CNAME + A, got %v", r.Answer)
	}
	if _, ok := r.Answer[0].(*dns.CNAME); !ok {
		t.Fatalf("first answer must be the CNAME: %v", r.Answer)
	}

	// The upstream target does not exist: NXDOMAIN with the upstream SOA,
	// TTL capped (the negative answer is cached for our name).
	r = query(t, addr, "gone.prod.internal.example.", dns.TypeA)
	if r.Rcode != dns.RcodeNameError || len(r.Answer) != 1 || len(r.Ns) != 1 {
		t.Fatalf("expected CNAME + NXDOMAIN with SOA, got %v", r)
	}
	if soa := r.Ns[0].(*dns.SOA); soa.Hdr.Ttl > negativeTTLCap {
		t.Fatalf("SOA TTL not capped: %v", soa)
	}

	// A CNAME to a name in our zone that we do not hold: chased upstream.
	r = query(t, addr, "topub.prod.internal.example.", dns.TypeA)
	if len(r.Answer) != 2 || r.Answer[1].(*dns.A).A.String() != "192.0.2.10" {
		t.Fatalf("expected CNAME + public A, got %v", r.Answer)
	}
}

func TestForwardAndCache(t *testing.T) {
	up, n := fakeUpstream(t)
	addr := startHorus(t, up)

	for i := 0; i < 3; i++ {
		r := query(t, addr, "ext.example.com.", dns.TypeA)
		if r.Authoritative || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
			t.Fatalf("forward: %v", r)
		}
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("expected 1 upstream query (cache), got %d", got)
	}

	// Negative answers are cached too.
	for i := 0; i < 2; i++ {
		if r := query(t, addr, "nope.example.com.", dns.TypeA); r.Rcode != dns.RcodeNameError {
			t.Fatalf("nxdomain forward: %v", r)
		}
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("expected 2 upstream queries total, got %d", got)
	}
}

func TestServfailWhenUpstreamDown(t *testing.T) {
	// Grab a free UDP port and close it so nothing answers there.
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	dead := pc.LocalAddr().String()
	pc.Close()
	addr := startHorus(t, dead)
	if r := query(t, addr, "anything.example.org.", dns.TypeA); r.Rcode != dns.RcodeServerFailure {
		t.Fatalf("expected SERVFAIL, got %v", r)
	}
}
