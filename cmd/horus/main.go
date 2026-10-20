// Command horus is the horus DNS server and its admin CLI in one binary.
//
//	horus serve                       run the server (configured via HORUS_* env)
//	horus zone|record|cluster ...     manage data through the REST API
package main

import (
	"fmt"
	"os"
	"strings"
)

// Set via -ldflags "-X main.version=...".
var version = "dev"

const usage = `horus — small internal DNS server

Usage:
  horus serve                              run the DNS + API server (config from HORUS_* env)

  horus [global flags] zone ls
  horus [global flags] zone add <zone> [--ttl N]
  horus [global flags] zone rm <zone>

  horus [global flags] record ls [zone]
  horus [global flags] record add <name> <A|AAAA|CNAME> <data> [--ttl N]
  horus [global flags] record rm <id>

  horus [global flags] cluster ls
  horus [global flags] cluster add <id> --zone <zone>     prints the cluster token once
  horus [global flags] cluster rotate <id>                prints the new token once
  horus [global flags] cluster rm <id>                    also removes the cluster's records

  horus [global flags] backup                             consistent DB copy on the server
  horus [global flags] whoami
  horus token                                             print a new random admin token
  horus version

Global flags (or env):
  --addr        API address (HORUS_ADDR, default unix:///run/horus/api.sock)
  --token-file  admin token file (HORUS_TOKEN_FILE, default /etc/horus/admin.token);
                HORUS_TOKEN overrides both
  --json        JSON output
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		// API errors already start with "horus: ".
		msg := err.Error()
		if !strings.HasPrefix(msg, "horus: ") {
			msg = "horus: " + msg
		}
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	switch args[0] {
	case "serve":
		return runServe()
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "token":
		tok, err := newAdminToken()
		if err != nil {
			return err
		}
		fmt.Println(tok)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	}
	return runCLI(args)
}
