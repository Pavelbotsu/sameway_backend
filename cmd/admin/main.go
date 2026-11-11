// Command admin is the operator's tool for the Sameway database: read
// reports, safety flags and bug reports, look up an account, suspend or
// unsuspend it, and run the retention job on demand. In production:
//
//	docker compose -f docker-compose.prod.yml exec app /admin reports
//	docker compose -f docker-compose.prod.yml exec app /admin suspend bad@example.com --reason "harassment"
//
// It reads the same DB_* environment variables as the server.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/handlers"
)

const usage = `usage: admin <command> [args]

  reports [-n 50]                   newest user reports
  flags   [-n 50]                   newest ride safety flags
  bugs    [-n 50]                   newest bug reports
  user <email|id>                   one account: status, rides, rating, reports
  suspend <email|id> --reason "..." block the account everywhere
  unsuspend <email|id>              lift a suspension
  retention                         run the data-retention job now
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	config.Load()
	if err := db.Connect(); err != nil {
		fmt.Fprintln(os.Stderr, "database:", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := run(ctx, os.Stdout, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run executes one command against db.DB, writing its output to out.
func run(ctx context.Context, out io.Writer, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	n := fs.Int("n", 50, "how many rows")
	reason := fs.String("reason", "", "why the account is suspended")
	// Allow flags after the positional argument (suspend x --reason y).
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	one := func() (string, error) {
		if len(pos) != 1 {
			return "", fmt.Errorf("%s needs one email or user id", cmd)
		}
		return pos[0], nil
	}

	switch cmd {
	case "reports":
		return listReports(ctx, db.DB, out, *n)
	case "flags":
		return listFlags(ctx, db.DB, out, *n)
	case "bugs":
		return listBugs(ctx, db.DB, out, *n)
	case "user":
		who, err := one()
		if err != nil {
			return err
		}
		return showUser(ctx, db.DB, out, who)
	case "suspend", "unsuspend":
		who, err := one()
		if err != nil {
			return err
		}
		return setSuspended(ctx, db.DB, out, who, cmd == "suspend", *reason)
	case "retention":
		counts := handlers.RunRetentionOnce(ctx)
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(out, "%-20s %d\n", k, counts[k])
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}
