package main

import (
	"reflect"
	"testing"

	"github.com/Warewave-Technology/horus/pkg/api"
)

func TestOwnerMatches(t *testing.T) {
	for _, tc := range []struct {
		owner, filter string
		want          bool
	}{
		{"manual", "", true},
		{"manual", "manual", true},
		{"prod/default/web", "prod", true},
		{"prod/default/web", "prod/default", true},
		{"prod/default/web", "prod/default/web", true},
		{"prod/default/web", "prod/default/we", false},
		{"production/default/web", "prod", false},
		{"manual", "prod", false},
	} {
		if got := ownerMatches(tc.owner, tc.filter); got != tc.want {
			t.Errorf("ownerMatches(%q, %q) = %v", tc.owner, tc.filter, got)
		}
	}
}

func TestSummarizeOwners(t *testing.T) {
	recs := []api.Record{
		{Zone: "p.", Name: "b.p.", Type: api.TypeA, Owner: "prod/default/web", UpdatedAt: 10},
		{Zone: "p.", Name: "a.p.", Type: api.TypeA, Owner: "prod/default/web", UpdatedAt: 30},
		{Zone: "p.", Name: "a.p.", Type: api.TypeAAAA, Owner: "prod/default/web", UpdatedAt: 20},
		{Zone: "p.", Name: "db.p.", Type: api.TypeA, Owner: "manual", UpdatedAt: 5},
		{Zone: "s.", Name: "x.s.", Type: api.TypeA, Owner: "manual", UpdatedAt: 7},
	}
	got := summarizeOwners(recs)
	want := []ownerSummary{
		{Owner: "manual", Zones: []string{"p.", "s."}, Names: []string{"db.p.", "x.s."}, Records: 2, UpdatedAt: 7},
		{Owner: "prod/default/web", Zones: []string{"p."}, Names: []string{"a.p.", "b.p."}, Records: 3, UpdatedAt: 30},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestAbbrev(t *testing.T) {
	if got := abbrev([]string{"a", "b"}, 3); got != "a,b" {
		t.Fatal(got)
	}
	if got := abbrev([]string{"a", "b", "c", "d", "e"}, 3); got != "a,b,c,... (+2)" {
		t.Fatal(got)
	}
}
