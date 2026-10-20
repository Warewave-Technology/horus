package store

import (
	"sort"

	"github.com/Warewave-Technology/horus/pkg/api"
)

// replacePlan is what ReplaceOwner will do inside its transaction.
type replacePlan struct {
	insert    []api.Record
	update    []api.Record // existing id, new ttl
	delete    []int64
	conflicts []api.Conflict
}

// planReplace computes the diff that makes owner's records equal to desired,
// honoring first-wins: a name already held by a different owner is skipped and
// reported as a conflict, and the owner's other names are still applied.
//
//   - desired: validated records (name/type/data normalized), all in one zone
//   - owned:   every record currently owned by owner
//   - byName:  every record (any owner) whose name appears in desired
//
// planReplace is pure so the first-wins rules can be unit-tested without SQLite.
func planReplace(owner string, desired, owned []api.Record, byName map[string][]api.Record) replacePlan {
	var p replacePlan

	// Decide which desired names we may write.
	allowed := map[string]bool{}
	names := map[string]bool{}
	for _, r := range desired {
		names[r.Name] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		holder := ""
		for _, ex := range byName[n] {
			if ex.Owner != owner {
				holder = ex.Owner
				break
			}
		}
		if holder != "" {
			p.conflicts = append(p.conflicts, api.Conflict{Name: n, HeldBy: holder})
			continue
		}
		allowed[n] = true
	}

	want := map[string]api.Record{}
	for _, r := range desired {
		if allowed[r.Name] {
			want[r.Key()] = r
		}
	}

	have := map[string]bool{}
	for _, ex := range owned {
		w, ok := want[ex.Key()]
		if !ok {
			p.delete = append(p.delete, ex.ID)
			continue
		}
		have[ex.Key()] = true
		if w.TTL != ex.TTL {
			ex.TTL = w.TTL
			p.update = append(p.update, ex)
		}
	}
	for _, r := range desired {
		if allowed[r.Name] && !have[r.Key()] {
			p.insert = append(p.insert, r)
			have[r.Key()] = true
		}
	}
	return p
}
