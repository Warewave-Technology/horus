package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Warewave-Technology/horus/pkg/api"
	"github.com/Warewave-Technology/horus/pkg/client"
)

type cli struct {
	c    *client.Client
	json bool
	out  io.Writer
}

func newAdminToken() (string, error) {
	b := make([]byte, 36)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func runCLI(args []string) error {
	gf := flag.NewFlagSet("horus", flag.ContinueOnError)
	addr := gf.String("addr", envOr("HORUS_ADDR", "unix:///run/horus/api.sock"), "API address")
	tokenFile := gf.String("token-file", envOr("HORUS_TOKEN_FILE", "/etc/horus/admin.token"), "admin token file")
	asJSON := gf.Bool("json", false, "JSON output")
	if err := gf.Parse(args); err != nil {
		return err
	}
	args = gf.Args()
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	token := os.Getenv("HORUS_TOKEN")
	if token == "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("read token: %w (set HORUS_TOKEN or --token-file)", err)
		}
		token = strings.TrimSpace(string(b))
	}
	c, err := client.New(*addr, token, client.Options{})
	if err != nil {
		return err
	}
	x := &cli{c: c, json: *asJSON, out: os.Stdout}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "zone", "zones":
		return x.zone(ctx, rest)
	case "record", "records", "rr":
		return x.record(ctx, rest)
	case "cluster", "clusters":
		return x.cluster(ctx, rest)
	case "owner", "owners":
		return x.owners(ctx, rest)
	case "backup":
		p, err := c.Backup(ctx)
		if err != nil {
			return err
		}
		return x.print(map[string]string{"path": p}, func(w io.Writer) { fmt.Fprintln(w, p) })
	case "whoami":
		w, err := c.WhoAmI(ctx)
		if err != nil {
			return err
		}
		return x.print(w, func(out io.Writer) {
			if w.Admin {
				fmt.Fprintln(out, "admin")
			} else {
				fmt.Fprintf(out, "cluster %s (zones %s)\n", w.Cluster, strings.Join(w.Zones, ", "))
			}
		})
	}
	return fmt.Errorf("unknown command %q (see `horus help`)", cmd)
}

// parse parses flags that may appear anywhere among positional args.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func need(pos []string, n int, usage string) error {
	if len(pos) != n {
		return fmt.Errorf("usage: horus %s", usage)
	}
	return nil
}

func (x *cli) print(v any, human func(io.Writer)) error {
	if x.json {
		enc := json.NewEncoder(x.out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	tw := tabwriter.NewWriter(x.out, 0, 4, 2, ' ', 0)
	human(tw)
	return tw.Flush()
}

func ts(unix int64) string {
	if unix == 0 {
		return "-"
	}
	return time.Unix(unix, 0).Local().Format("2006-01-02 15:04")
}

// ---------------------------------------------------------------- zone

func (x *cli) zone(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	fs := flag.NewFlagSet("zone", flag.ContinueOnError)
	ttl := fs.Uint("ttl", 0, "default TTL (seconds)")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	switch args[0] {
	case "ls", "list":
		zones, err := x.c.ListZones(ctx)
		if err != nil {
			return err
		}
		return x.print(zones, func(w io.Writer) {
			fmt.Fprintln(w, "ZONE\tDEFAULT TTL\tSERIAL\tUPDATED")
			for _, z := range zones {
				fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", z.Name, z.DefaultTTL, z.Serial, ts(z.UpdatedAt))
			}
		})
	case "add", "create":
		if err := need(pos, 1, "zone add <zone> [--ttl N]"); err != nil {
			return err
		}
		z, err := x.c.CreateZone(ctx, pos[0], uint32(*ttl))
		if err != nil {
			return err
		}
		return x.print(z, func(w io.Writer) { fmt.Fprintf(w, "created zone %s (default ttl %d)\n", z.Name, z.DefaultTTL) })
	case "rm", "delete":
		if err := need(pos, 1, "zone rm <zone>"); err != nil {
			return err
		}
		if err := x.c.DeleteZone(ctx, pos[0]); err != nil {
			return err
		}
		return x.print(map[string]string{"deleted": pos[0]}, func(w io.Writer) { fmt.Fprintf(w, "deleted zone %s\n", pos[0]) })
	}
	return fmt.Errorf("unknown zone subcommand %q", args[0])
}

// ---------------------------------------------------------------- record

func (x *cli) record(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	ttl := fs.Uint("ttl", 0, "TTL in seconds (0 = zone default)")
	owner := fs.String("owner", "", "only records of this owner (manual, <cluster>, <cluster>/<ns>[/<name>])")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	switch args[0] {
	case "ls", "list":
		if len(pos) > 1 {
			return fmt.Errorf("usage: horus record ls [zone] [--owner OWNER]")
		}
		all, err := x.records(ctx, pos)
		if err != nil {
			return err
		}
		recs := []api.Record{}
		for _, r := range all {
			if ownerMatches(r.Owner, *owner) {
				recs = append(recs, r)
			}
		}
		return x.print(recs, func(w io.Writer) {
			fmt.Fprintln(w, "ID\tNAME\tTYPE\tDATA\tTTL\tOWNER\tUPDATED")
			for _, r := range recs {
				t := "default"
				if r.TTL > 0 {
					t = strconv.Itoa(int(r.TTL))
				}
				fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Name, r.Type, r.Data, t, r.Owner, ts(r.UpdatedAt))
			}
		})
	case "add", "create":
		if err := need(pos, 3, "record add <name> <A|AAAA|CNAME> <data> [--ttl N]"); err != nil {
			return err
		}
		name := api.NormalizeName(pos[0])
		zone, err := x.zoneFor(ctx, name)
		if err != nil {
			return err
		}
		r, err := x.c.AddRecord(ctx, zone, api.Record{Name: name, Type: api.RecordType(pos[1]), Data: pos[2], TTL: uint32(*ttl)})
		if err != nil {
			return conflictHint(err)
		}
		return x.print(r, func(w io.Writer) { fmt.Fprintf(w, "created #%d %s %s %s\n", r.ID, r.Name, r.Type, r.Data) })
	case "rm", "delete":
		if err := need(pos, 1, "record rm <id>"); err != nil {
			return err
		}
		id, err := strconv.ParseInt(pos[0], 10, 64)
		if err != nil {
			return fmt.Errorf("record id must be a number (see `horus record ls`)")
		}
		if err := x.c.DeleteRecord(ctx, id); err != nil {
			return conflictHint(err)
		}
		return x.print(map[string]int64{"deleted": id}, func(w io.Writer) { fmt.Fprintf(w, "deleted record #%d\n", id) })
	}
	return fmt.Errorf("unknown record subcommand %q", args[0])
}

// records returns the records of the given zone, or of every zone when none
// is given.
func (x *cli) records(ctx context.Context, zone []string) ([]api.Record, error) {
	zones := zone
	if len(zones) == 0 {
		all, err := x.c.ListZones(ctx)
		if err != nil {
			return nil, err
		}
		for _, z := range all {
			zones = append(zones, z.Name)
		}
	}
	var recs []api.Record
	for _, z := range zones {
		rs, err := x.c.ListRecords(ctx, z)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rs...)
	}
	return recs, nil
}

// ownerMatches reports whether owner is filter or lies below it: "prod"
// matches every owner of cluster prod, "prod/default" one namespace.
func ownerMatches(owner, filter string) bool {
	return filter == "" || owner == filter || strings.HasPrefix(owner, filter+"/")
}

// zoneFor finds the most specific existing zone containing name.
func (x *cli) zoneFor(ctx context.Context, name string) (string, error) {
	zones, err := x.c.ListZones(ctx)
	if err != nil {
		return "", err
	}
	best := ""
	for _, z := range zones {
		if api.InZone(name, z.Name) && len(z.Name) > len(best) {
			best = z.Name
		}
	}
	if best == "" {
		return "", fmt.Errorf("no zone contains %s (create it with `horus zone add`)", name)
	}
	return best, nil
}

func conflictHint(err error) error {
	var ae *client.APIError
	if errors.As(err, &ae) && len(ae.Conflicts()) > 0 {
		var parts []string
		for _, c := range ae.Conflicts() {
			parts = append(parts, fmt.Sprintf("%s is owned by %s", c.Name, c.HeldBy))
		}
		return fmt.Errorf("conflict (first-wins): %s", strings.Join(parts, "; "))
	}
	return err
}

// listFlag is a repeatable flag that also splits comma-separated values.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			*l = append(*l, item)
		}
	}
	return nil
}

// ---------------------------------------------------------------- owners

// ownerSummary is one row of `horus owners`.
type ownerSummary struct {
	Owner     string   `json:"owner"`
	Zones     []string `json:"zones"`
	Names     []string `json:"names"`
	Records   int      `json:"records"`
	UpdatedAt int64    `json:"updated_at"`
}

// summarizeOwners groups records by owner, sorted by owner.
func summarizeOwners(recs []api.Record) []ownerSummary {
	by := map[string]*ownerSummary{}
	zones := map[string]map[string]bool{}
	names := map[string]map[string]bool{}
	for _, r := range recs {
		s := by[r.Owner]
		if s == nil {
			s = &ownerSummary{Owner: r.Owner}
			by[r.Owner] = s
			zones[r.Owner], names[r.Owner] = map[string]bool{}, map[string]bool{}
		}
		s.Records++
		s.UpdatedAt = max(s.UpdatedAt, r.UpdatedAt)
		zones[r.Owner][r.Zone] = true
		names[r.Owner][r.Name] = true
	}
	out := make([]ownerSummary, 0, len(by))
	for o, s := range by {
		s.Zones = slices.Sorted(maps.Keys(zones[o]))
		s.Names = slices.Sorted(maps.Keys(names[o]))
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b ownerSummary) int { return strings.Compare(a.Owner, b.Owner) })
	return out
}

// abbrev joins at most n items and says how many were left out.
func abbrev(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ",")
	}
	return fmt.Sprintf("%s,... (+%d)", strings.Join(items[:n], ","), len(items)-n)
}

func (x *cli) owners(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("owners", flag.ContinueOnError)
	owner := fs.String("owner", "", "only this owner (manual, <cluster>, <cluster>/<ns>[/<name>])")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fmt.Errorf("usage: horus owners [zone] [--owner OWNER]")
	}
	all, err := x.records(ctx, pos)
	if err != nil {
		return err
	}
	var recs []api.Record
	for _, r := range all {
		if ownerMatches(r.Owner, *owner) {
			recs = append(recs, r)
		}
	}
	sum := summarizeOwners(recs)
	return x.print(sum, func(w io.Writer) {
		fmt.Fprintln(w, "OWNER\tRECORDS\tNAMES\tUPDATED")
		for _, s := range sum {
			fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", s.Owner, s.Records, abbrev(s.Names, 3), ts(s.UpdatedAt))
		}
	})
}

// ---------------------------------------------------------------- cluster

func (x *cli) cluster(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	if args[0] == "zone" || args[0] == "zones" {
		return x.clusterZone(ctx, args[1:])
	}
	fs := flag.NewFlagSet("cluster", flag.ContinueOnError)
	var zones listFlag
	fs.Var(&zones, "zone", "zone the cluster may write to (repeatable, or comma-separated)")
	pos, err := parse(fs, args[1:])
	if err != nil {
		return err
	}
	switch args[0] {
	case "ls", "list":
		cs, err := x.c.ListClusters(ctx)
		if err != nil {
			return err
		}
		return x.print(cs, func(w io.Writer) {
			fmt.Fprintln(w, "CLUSTER\tZONES\tCREATED\tLAST SEEN")
			for _, c := range cs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.ID, strings.Join(c.Zones, ","), ts(c.CreatedAt), ts(c.LastSeen))
			}
		})
	case "add", "create":
		if err := need(pos, 1, "cluster add <id> --zone <zone> [--zone <zone>...]"); err != nil {
			return err
		}
		if len(zones) == 0 {
			return fmt.Errorf("at least one --zone is required")
		}
		c, err := x.c.CreateCluster(ctx, pos[0], zones)
		if err != nil {
			return err
		}
		return x.print(c, func(w io.Writer) {
			fmt.Fprintf(w, "created cluster %s for zones %s\n", c.ID, strings.Join(c.Zones, ", "))
			fmt.Fprintf(w, "token (shown once, store it in the operator Secret):\n%s\n", c.Token)
		})
	case "rotate", "rotate-token":
		if err := need(pos, 1, "cluster rotate <id>"); err != nil {
			return err
		}
		tok, err := x.c.RotateClusterToken(ctx, pos[0])
		if err != nil {
			return err
		}
		return x.print(api.Cluster{ID: pos[0], Token: tok}, func(w io.Writer) {
			fmt.Fprintf(w, "new token for %s (shown once; the old token no longer works):\n%s\n", pos[0], tok)
		})
	case "rm", "delete":
		if err := need(pos, 1, "cluster rm <id>"); err != nil {
			return err
		}
		if err := x.c.DeleteCluster(ctx, pos[0]); err != nil {
			return err
		}
		return x.print(map[string]string{"deleted": pos[0]}, func(w io.Writer) {
			fmt.Fprintf(w, "deleted cluster %s and its records\n", pos[0])
		})
	}
	return fmt.Errorf("unknown cluster subcommand %q", args[0])
}

// clusterZone handles `horus cluster zone add|rm <id> <zone>`.
func (x *cli) clusterZone(ctx context.Context, args []string) error {
	const usage = "cluster zone add|rm <cluster> <zone>"
	if len(args) != 3 {
		return fmt.Errorf("usage: horus %s", usage)
	}
	id, zone := args[1], args[2]
	switch args[0] {
	case "add":
		c, err := x.c.AddClusterZone(ctx, id, zone)
		if err != nil {
			return err
		}
		return x.print(c, func(w io.Writer) {
			fmt.Fprintf(w, "cluster %s now has zones %s\n", c.ID, strings.Join(c.Zones, ", "))
		})
	case "rm", "remove", "delete":
		n, err := x.c.RemoveClusterZone(ctx, id, zone)
		if err != nil {
			return err
		}
		return x.print(api.RemoveClusterZoneResponse{RecordsRemoved: n}, func(w io.Writer) {
			fmt.Fprintf(w, "removed zone %s from cluster %s (%d of its records deleted)\n", api.NormalizeName(zone), id, n)
		})
	}
	return fmt.Errorf("usage: horus %s", usage)
}
