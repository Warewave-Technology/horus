// Package ingressdns turns an annotated Ingress into the record set horus
// should hold for it. It is pure (stdlib only, no Kubernetes types) so the
// rules are unit-tested here and the operator only does the plumbing.
package ingressdns

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/Warewave-Technology/horus/pkg/api"
)

// Annotation keys. All live under one prefix.
const (
	Prefix = "horus.warewave.tech/"

	// AnnotationRegister opts an Ingress in. Value "true" (case-insensitive).
	AnnotationRegister = Prefix + "register"
	// AnnotationTTL sets the TTL in seconds for all records of the Ingress.
	AnnotationTTL = Prefix + "ttl"
	// AnnotationHostnames replaces spec.rules[].host with a comma-separated list.
	AnnotationHostnames = Prefix + "hostnames"
	// AnnotationTarget replaces status.loadBalancer with a comma-separated list
	// of IPs (A/AAAA) or a single hostname (CNAME).
	AnnotationTarget = Prefix + "target"
)

// LBEntry mirrors networking/v1 IngressLoadBalancerIngress (ip or hostname).
type LBEntry struct {
	IP       string
	Hostname string
}

// Input is the part of an Ingress that matters.
type Input struct {
	Annotations  map[string]string
	Hosts        []string  // spec.rules[].host, in order, may contain duplicates/empties
	LoadBalancer []LBEntry // status.loadBalancer.ingress
}

// Skip explains why a host was not registered.
type Skip struct {
	Host   string
	Reason string
}

// Plan is the desired state for one Ingress.
type Plan struct {
	Records []api.Record
	Skipped []Skip
	// Pending is true when no target is known yet (empty LB status and no
	// target annotation). The caller should leave existing records untouched
	// and retry later instead of deleting them.
	Pending bool
}

// Enabled reports whether the Ingress opted in.
func Enabled(ann map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(ann[AnnotationRegister]), "true")
}

// Desired computes the plan for an Ingress of a cluster that may write to
// zones (normalized FQDNs). Hosts outside every zone are skipped. It returns
// an error only for malformed annotations, which the operator reports as an
// event; nothing is changed on the server in that case.
func Desired(in Input, zones []string) (Plan, error) {
	var p Plan

	var ttl uint32
	if v := strings.TrimSpace(in.Annotations[AnnotationTTL]); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil || n == 0 || n > api.MaxTTL {
			return p, fmt.Errorf("annotation %s=%q: must be 1..%d", AnnotationTTL, v, api.MaxTTL)
		}
		ttl = uint32(n)
	}

	hosts := in.Hosts
	if v, ok := in.Annotations[AnnotationHostnames]; ok {
		hosts = splitList(v)
		if len(hosts) == 0 {
			return p, fmt.Errorf("annotation %s is empty", AnnotationHostnames)
		}
	}

	targets, err := resolveTargets(in)
	if err != nil {
		return p, err
	}

	seen := map[string]bool{}
	var names []string
	zoneOf := map[string]string{}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue // a rule without host (default backend)
		}
		name := api.NormalizeName(h)
		if seen[name] {
			continue
		}
		seen[name] = true
		zone := api.ZoneFor(name, zones)
		switch {
		case strings.HasPrefix(name, "*."):
			p.Skipped = append(p.Skipped, Skip{Host: h, Reason: "wildcard hosts are not supported"})
		case zone == "":
			p.Skipped = append(p.Skipped, Skip{Host: h, Reason: outsideReason(zones)})
		case name == zone && len(targets) == 1 && targets[0].Type == api.TypeCNAME:
			p.Skipped = append(p.Skipped, Skip{Host: h, Reason: "CNAME is not allowed at the zone apex"})
		default:
			names = append(names, name)
			zoneOf[name] = zone
		}
	}
	sort.Strings(names)

	if len(names) > 0 && len(targets) == 0 {
		p.Pending = true
		return p, nil
	}
	for _, n := range names {
		for _, t := range targets {
			p.Records = append(p.Records, api.Record{Name: n, Type: t.Type, Data: t.Data, TTL: ttl})
		}
	}
	// Final safety net: the server would reject these anyway.
	for i := range p.Records {
		if err := api.ValidateRecord(&p.Records[i], zoneOf[p.Records[i].Name]); err != nil {
			return Plan{}, err
		}
	}
	return p, nil
}

func outsideReason(zones []string) string {
	switch len(zones) {
	case 0:
		return "the cluster has no zones"
	case 1:
		return "outside zone " + zones[0]
	}
	return "outside the cluster's zones " + strings.Join(zones, ", ")
}

type target struct {
	Type api.RecordType
	Data string
}

// resolveTargets picks the record targets: annotation override first, then LB
// status. IPs win over hostnames when both are present (a CNAME cannot coexist
// with A/AAAA on the same name).
func resolveTargets(in Input) ([]target, error) {
	var ips []string
	var hostnames []string
	if v, ok := in.Annotations[AnnotationTarget]; ok {
		for _, t := range splitList(v) {
			if _, err := netip.ParseAddr(t); err == nil {
				ips = append(ips, t)
			} else {
				hostnames = append(hostnames, t)
			}
		}
		if len(ips) == 0 && len(hostnames) == 0 {
			return nil, fmt.Errorf("annotation %s is empty", AnnotationTarget)
		}
		if len(ips) > 0 && len(hostnames) > 0 {
			return nil, fmt.Errorf("annotation %s mixes IPs and hostnames", AnnotationTarget)
		}
		if len(hostnames) > 1 {
			return nil, fmt.Errorf("annotation %s: at most one hostname (CNAME)", AnnotationTarget)
		}
	} else {
		for _, e := range in.LoadBalancer {
			if e.IP != "" {
				ips = append(ips, e.IP)
			} else if e.Hostname != "" {
				hostnames = append(hostnames, e.Hostname)
			}
		}
	}

	var out []target
	seen := map[string]bool{}
	for _, s := range ips {
		ip, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("invalid IP %q", s)
		}
		ip = ip.Unmap()
		t := target{Type: api.TypeA, Data: ip.String()}
		if ip.Is6() {
			t.Type = api.TypeAAAA
		}
		if !seen[t.Data] {
			seen[t.Data] = true
			out = append(out, t)
		}
	}
	if len(out) > 0 {
		sort.Slice(out, func(i, j int) bool { return out[i].Data < out[j].Data })
		return out, nil
	}
	if len(hostnames) > 0 {
		// Several LB hostnames cannot be expressed as one CNAME; use the first
		// (sorted, for stability).
		sort.Strings(hostnames)
		return []target{{Type: api.TypeCNAME, Data: api.NormalizeName(hostnames[0])}}, nil
	}
	return nil, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
