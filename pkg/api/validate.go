package api

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// MaxTTL is the largest TTL horus accepts on a record or zone.
const MaxTTL = 86400

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("invalid")

func invalidf(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

var labelRe = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$`)

// NormalizeName lowercases a domain name and adds the trailing dot.
func NormalizeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name != "" && !strings.HasSuffix(name, ".") {
		name += "."
	}
	return name
}

// ValidateName checks that name is a syntactically valid, normalized FQDN.
// Wildcards are not supported.
func ValidateName(name string) error {
	if name == "" || name == "." {
		return invalidf("empty name")
	}
	if name != NormalizeName(name) {
		return invalidf("name %q is not normalized (lowercase, trailing dot)", name)
	}
	if len(name) > 254 {
		return invalidf("name %q is longer than 253 characters", name)
	}
	for _, l := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if l == "*" {
			return invalidf("wildcard names are not supported: %q", name)
		}
		if !labelRe.MatchString(l) {
			return invalidf("invalid label %q in %q", l, name)
		}
	}
	return nil
}

// InZone reports whether name equals zone or is below it. Both must be normalized.
func InZone(name, zone string) bool {
	return name == zone || strings.HasSuffix(name, "."+zone)
}

// ZoneFor returns the most specific zone in zones that contains name, or "".
// name and zones must be normalized.
func ZoneFor(name string, zones []string) string {
	best := ""
	for _, z := range zones {
		if InZone(name, z) && len(z) > len(best) {
			best = z
		}
	}
	return best
}

var clusterIDRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateClusterID checks a cluster id (DNS-label-like, max 63 chars).
func ValidateClusterID(id string) error {
	if !clusterIDRe.MatchString(id) {
		return invalidf("cluster id %q must match %s", id, clusterIDRe)
	}
	return nil
}

// ValidateZone normalizes and validates a zone in place.
func ValidateZone(z *Zone) error {
	z.Name = NormalizeName(z.Name)
	if err := ValidateName(z.Name); err != nil {
		return err
	}
	if z.DefaultTTL > MaxTTL {
		return invalidf("default_ttl %d exceeds %d", z.DefaultTTL, MaxTTL)
	}
	return nil
}

// ValidateRecord normalizes and validates a single record in place against the
// zone it will live in.
func ValidateRecord(r *Record, zone string) error {
	r.Name = NormalizeName(r.Name)
	r.Type = RecordType(strings.ToUpper(string(r.Type)))
	r.Data = strings.TrimSpace(r.Data)
	if err := ValidateName(r.Name); err != nil {
		return err
	}
	if !InZone(r.Name, zone) {
		return invalidf("name %q is outside zone %q", r.Name, zone)
	}
	if r.TTL > MaxTTL {
		return invalidf("ttl %d exceeds %d", r.TTL, MaxTTL)
	}
	switch r.Type {
	case TypeA:
		ip, err := netip.ParseAddr(r.Data)
		if err != nil || !ip.Is4() {
			return invalidf("A record %q needs an IPv4 address, got %q", r.Name, r.Data)
		}
		r.Data = ip.String()
	case TypeAAAA:
		ip, err := netip.ParseAddr(r.Data)
		if err != nil || !ip.Is6() || ip.Is4In6() || ip.Zone() != "" {
			return invalidf("AAAA record %q needs an IPv6 address, got %q", r.Name, r.Data)
		}
		r.Data = ip.String()
	case TypeCNAME:
		if r.Name == zone {
			return invalidf("CNAME is not allowed at the zone apex %q", zone)
		}
		r.Data = NormalizeName(r.Data)
		if err := ValidateName(r.Data); err != nil {
			return invalidf("CNAME target: %v", err)
		}
		if r.Data == r.Name {
			return invalidf("CNAME %q points to itself", r.Name)
		}
	default:
		return invalidf("unsupported record type %q (A, AAAA, CNAME)", r.Type)
	}
	return nil
}

// ValidateRRSet checks the rules that span records of the same name:
// a CNAME cannot coexist with any other record, and there is at most one CNAME.
// Records must already be validated individually.
func ValidateRRSet(recs []Record) error {
	byName := map[string][]Record{}
	for _, r := range recs {
		byName[r.Name] = append(byName[r.Name], r)
	}
	for name, set := range byName {
		cnames := 0
		seen := map[string]bool{}
		for _, r := range set {
			if r.Type == TypeCNAME {
				cnames++
			}
			if seen[r.Key()] {
				return invalidf("duplicate record %s", r.Key())
			}
			seen[r.Key()] = true
		}
		if cnames > 1 {
			return invalidf("%q has more than one CNAME", name)
		}
		if cnames == 1 && len(set) > 1 {
			return invalidf("%q has a CNAME and other records", name)
		}
	}
	return nil
}
