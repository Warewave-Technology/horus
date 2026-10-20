package api

import (
	"errors"
	"testing"
)

func TestValidateRecord(t *testing.T) {
	const zone = "prod.internal.example."
	cases := []struct {
		name    string
		rec     Record
		wantErr bool
		want    Record
	}{
		{"A ok + normalize", Record{Name: "App.Prod.Internal.Example", Type: "a", Data: " 10.0.0.1 "}, false,
			Record{Name: "app.prod.internal.example.", Type: TypeA, Data: "10.0.0.1"}},
		{"A with v6", Record{Name: "app.prod.internal.example.", Type: TypeA, Data: "::1"}, true, Record{}},
		{"AAAA ok", Record{Name: "app.prod.internal.example.", Type: TypeAAAA, Data: "2001:DB8::1"}, false,
			Record{Name: "app.prod.internal.example.", Type: TypeAAAA, Data: "2001:db8::1"}},
		{"AAAA with v4-mapped", Record{Name: "app.prod.internal.example.", Type: TypeAAAA, Data: "::ffff:10.0.0.1"}, true, Record{}},
		{"CNAME ok", Record{Name: "www.prod.internal.example.", Type: TypeCNAME, Data: "LB.Example.com"}, false,
			Record{Name: "www.prod.internal.example.", Type: TypeCNAME, Data: "lb.example.com."}},
		{"CNAME at apex", Record{Name: zone, Type: TypeCNAME, Data: "lb.example.com."}, true, Record{}},
		{"CNAME to self", Record{Name: "a.prod.internal.example.", Type: TypeCNAME, Data: "a.prod.internal.example"}, true, Record{}},
		{"outside zone", Record{Name: "app.staging.internal.example.", Type: TypeA, Data: "10.0.0.1"}, true, Record{}},
		{"suffix trick", Record{Name: "evilprod.internal.example.", Type: TypeA, Data: "10.0.0.1"}, true, Record{}},
		{"wildcard", Record{Name: "*.prod.internal.example.", Type: TypeA, Data: "10.0.0.1"}, true, Record{}},
		{"bad label", Record{Name: "-x.prod.internal.example.", Type: TypeA, Data: "10.0.0.1"}, true, Record{}},
		{"unsupported type", Record{Name: "x.prod.internal.example.", Type: "TXT", Data: "hi"}, true, Record{}},
		{"ttl too big", Record{Name: "x.prod.internal.example.", Type: TypeA, Data: "10.0.0.1", TTL: MaxTTL + 1}, true, Record{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.rec
			err := ValidateRecord(&r, zone)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got none (%+v)", r)
				}
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("error does not wrap ErrInvalid: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.Name != tc.want.Name || r.Type != tc.want.Type || r.Data != tc.want.Data {
				t.Fatalf("got %+v, want %+v", r, tc.want)
			}
		})
	}
}

func TestValidateRRSet(t *testing.T) {
	a := Record{Name: "x.z.", Type: TypeA, Data: "10.0.0.1"}
	a2 := Record{Name: "x.z.", Type: TypeA, Data: "10.0.0.2"}
	aaaa := Record{Name: "x.z.", Type: TypeAAAA, Data: "2001:db8::1"}
	c := Record{Name: "x.z.", Type: TypeCNAME, Data: "y.z."}
	c2 := Record{Name: "x.z.", Type: TypeCNAME, Data: "w.z."}

	if err := ValidateRRSet([]Record{a, a2, aaaa}); err != nil {
		t.Fatalf("multi A/AAAA should be fine: %v", err)
	}
	if err := ValidateRRSet([]Record{c}); err != nil {
		t.Fatalf("single CNAME should be fine: %v", err)
	}
	for name, set := range map[string][]Record{
		"cname+a":  {c, a},
		"2 cnames": {c, c2},
		"dup":      {a, a},
	} {
		if err := ValidateRRSet(set); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestInZone(t *testing.T) {
	if !InZone("a.b.example.", "b.example.") || !InZone("b.example.", "b.example.") {
		t.Fatal("expected in zone")
	}
	if InZone("ab.example.", "b.example.") {
		t.Fatal("suffix without dot must not match")
	}
}

func TestZoneFor(t *testing.T) {
	zones := []string{"a.example.", "b.example.", "x.a.example."}
	for name, want := range map[string]string{
		"a.example.":        "a.example.",
		"w.a.example.":      "a.example.",
		"w.x.a.example.":    "x.a.example.",
		"b.example.":        "b.example.",
		"c.example.":        "",
		"aa.example.":       "",
		"w.b.example.extra": "",
	} {
		if got := ZoneFor(name, zones); got != want {
			t.Errorf("ZoneFor(%q) = %q, want %q", name, got, want)
		}
	}
	if ZoneFor("a.example.", nil) != "" {
		t.Error("no zones")
	}
}
