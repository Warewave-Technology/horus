package ingressdns

import (
	"strings"
	"testing"

	"github.com/Warewave-Technology/horus/pkg/api"
)

var zones = []string{"prod.internal.example."}

func on(extra map[string]string) map[string]string {
	m := map[string]string{AnnotationRegister: "true"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestEnabled(t *testing.T) {
	if !Enabled(map[string]string{AnnotationRegister: " TRUE "}) {
		t.Fatal("expected enabled")
	}
	for _, v := range []string{"", "false", "yes", "1"} {
		if Enabled(map[string]string{AnnotationRegister: v}) {
			t.Fatalf("%q must not enable", v)
		}
	}
	if Enabled(nil) {
		t.Fatal("nil annotations")
	}
}

func TestDesiredFromLBIPs(t *testing.T) {
	p, err := Desired(Input{
		Annotations:  on(map[string]string{AnnotationTTL: "60"}),
		Hosts:        []string{"web.prod.internal.example", "API.prod.internal.example", "web.prod.internal.example", ""},
		LoadBalancer: []LBEntry{{IP: "10.0.0.2"}, {IP: "2001:db8::1"}, {IP: "10.0.0.2"}},
	}, zones)
	if err != nil {
		t.Fatal(err)
	}
	if p.Pending || len(p.Skipped) != 0 {
		t.Fatalf("%+v", p)
	}
	// 2 unique names x (A + AAAA)
	if len(p.Records) != 4 {
		t.Fatalf("records: %+v", p.Records)
	}
	want := []string{
		"api.prod.internal.example. A 10.0.0.2",
		"api.prod.internal.example. AAAA 2001:db8::1",
		"web.prod.internal.example. A 10.0.0.2",
		"web.prod.internal.example. AAAA 2001:db8::1",
	}
	for i, r := range p.Records {
		if r.Key() != want[i] || r.TTL != 60 {
			t.Fatalf("record %d: %s ttl=%d, want %s", i, r.Key(), r.TTL, want[i])
		}
	}
}

func TestDesiredFromLBHostname(t *testing.T) {
	p, err := Desired(Input{
		Annotations:  on(nil),
		Hosts:        []string{"web.prod.internal.example", "prod.internal.example"},
		LoadBalancer: []LBEntry{{Hostname: "abc.elb.amazonaws.com"}},
	}, zones)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Records) != 1 || p.Records[0].Type != api.TypeCNAME || p.Records[0].Data != "abc.elb.amazonaws.com." {
		t.Fatalf("records: %+v", p.Records)
	}
	if len(p.Skipped) != 1 || p.Skipped[0].Host != "prod.internal.example" {
		t.Fatalf("apex CNAME must be skipped: %+v", p.Skipped)
	}
}

func TestDesiredSkipsAndPending(t *testing.T) {
	p, err := Desired(Input{
		Annotations: on(nil),
		Hosts:       []string{"*.prod.internal.example", "web.staging.internal.example", "web.prod.internal.example"},
	}, zones)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Pending || len(p.Records) != 0 || len(p.Skipped) != 2 {
		t.Fatalf("%+v", p)
	}

	// Nothing in zone: not pending, just empty (operator will clear records).
	p, err = Desired(Input{Annotations: on(nil), Hosts: []string{"x.other.example"}}, zones)
	if err != nil || p.Pending || len(p.Records) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestDesiredAnnotationOverrides(t *testing.T) {
	p, err := Desired(Input{
		Annotations: on(map[string]string{
			AnnotationHostnames: "a.prod.internal.example, b.prod.internal.example",
			AnnotationTarget:    "192.168.10.5",
		}),
		Hosts:        []string{"ignored.prod.internal.example"},
		LoadBalancer: []LBEntry{{IP: "10.0.0.1"}},
	}, zones)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Records) != 2 || p.Records[0].Data != "192.168.10.5" || p.Records[0].Name != "a.prod.internal.example." {
		t.Fatalf("%+v", p.Records)
	}
}

func TestDesiredBadAnnotations(t *testing.T) {
	base := Input{Hosts: []string{"a.prod.internal.example"}, LoadBalancer: []LBEntry{{IP: "10.0.0.1"}}}
	for name, ann := range map[string]map[string]string{
		"ttl zero":        {AnnotationTTL: "0"},
		"ttl junk":        {AnnotationTTL: "fast"},
		"ttl too big":     {AnnotationTTL: "999999"},
		"empty hostnames": {AnnotationHostnames: " , "},
		"mixed target":    {AnnotationTarget: "10.0.0.1,lb.example.com"},
		"two hostnames":   {AnnotationTarget: "a.example.com,b.example.com"},
		"empty target":    {AnnotationTarget: ""},
	} {
		in := base
		in.Annotations = on(ann)
		if _, err := Desired(in, zones); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestIPsWinOverHostnames(t *testing.T) {
	p, err := Desired(Input{
		Annotations:  on(nil),
		Hosts:        []string{"a.prod.internal.example"},
		LoadBalancer: []LBEntry{{Hostname: "lb.example.com"}, {IP: "10.0.0.1"}},
	}, zones)
	if err != nil || len(p.Records) != 1 || p.Records[0].Type != api.TypeA {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestDesiredSeveralZones(t *testing.T) {
	multi := []string{"edge.example.", "prod.internal.example."}
	p, err := Desired(Input{
		Annotations:  on(nil),
		Hosts:        []string{"web.prod.internal.example", "web.edge.example", "edge.example", "x.other.example"},
		LoadBalancer: []LBEntry{{IP: "10.0.0.1"}},
	}, multi)
	if err != nil {
		t.Fatal(err)
	}
	// Records carry no zone: the server puts each into the cluster zone that
	// contains it.
	var got []string
	for _, r := range p.Records {
		got = append(got, r.Key())
	}
	want := []string{
		"edge.example. A 10.0.0.1",
		"web.edge.example. A 10.0.0.1",
		"web.prod.internal.example. A 10.0.0.1",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("records:\n got %q\nwant %q", got, want)
	}
	if len(p.Skipped) != 1 || p.Skipped[0].Host != "x.other.example" ||
		p.Skipped[0].Reason != "outside the cluster's zones edge.example., prod.internal.example." {
		t.Fatalf("skipped: %+v", p.Skipped)
	}

	// The apex rule applies per zone.
	p, err = Desired(Input{
		Annotations:  on(nil),
		Hosts:        []string{"edge.example", "web.edge.example"},
		LoadBalancer: []LBEntry{{Hostname: "lb.example.com"}},
	}, multi)
	if err != nil || len(p.Records) != 1 || p.Records[0].Name != "web.edge.example." || len(p.Skipped) != 1 {
		t.Fatalf("apex: %+v %v", p, err)
	}

	// No zones at all: everything is skipped.
	p, err = Desired(Input{Annotations: on(nil), Hosts: []string{"a.example"}, LoadBalancer: []LBEntry{{IP: "10.0.0.1"}}}, nil)
	if err != nil || len(p.Records) != 0 || p.Skipped[0].Reason != "the cluster has no zones" {
		t.Fatalf("no zones: %+v %v", p, err)
	}
}
