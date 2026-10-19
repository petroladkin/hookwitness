package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/petroladkin/hookwitness/internal/config"
	"github.com/petroladkin/hookwitness/internal/store"
)

const dbUsage = `Usage: hookwitness db compact [-db PATH] [-keep-old]

Rewrites the database without its free space and so that from then on
clearing and pruning give space back to the file system. Run it with the
server stopped (it refuses while anything has the database open). It needs
free disk for the data the database holds; on any failure the database is
left as it was. -keep-old keeps the old file as <db>.before-compact.

Exit codes: 0 done, 1 failed (the database is unchanged), 2 usage.
`

func dbCmd(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "compact" {
		fmt.Fprint(stderr, dbUsage)
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			return 0
		}
		return 2
	}
	fs := flag.NewFlagSet("db compact", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	db := fs.String("db", envOr(getenv, "HOOKWITNESS_DB", config.DefaultDBPath), "path to the sqlite database file (env HOOKWITNESS_DB)")
	keep := fs.Bool("keep-old", false, "keep the old file as <db>.before-compact")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stderr, dbUsage)
			return 0
		}
		fmt.Fprintf(stderr, "hookwitness db compact: %v\n\n%s", err, dbUsage)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "hookwitness db compact: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if _, err := os.Stat(*db); err != nil {
		fmt.Fprintf(stderr, "hookwitness db compact: no database at %s: give -db\n", *db)
		return 1
	}
	res, err := store.Compact(ctx, *db, *keep)
	if err != nil {
		fmt.Fprintf(stderr, "hookwitness db compact: %v\n", err)
		return 1
	}
	what := "compacted"
	if res.Converted {
		what = "converted and compacted"
	}
	fmt.Fprintf(stdout, "%s %s: %s → %s in %s\n", *db, what, mib(res.Before), mib(res.After), res.Took.Round(1e6))
	if *keep {
		fmt.Fprintf(stdout, "the old file is %s.before-compact: remove it to free its space\n", *db)
	}
	return 0
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
