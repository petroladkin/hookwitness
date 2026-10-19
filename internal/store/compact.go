package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

// CompactResult is what Compact did.
type CompactResult struct {
	Before, After int64 // the file's size (with its WAL) before and after
	Converted     bool  // auto_vacuum was not incremental before
	Took          time.Duration
}

// compactTemp is where Compact writes the new file (a variable for tests).
var compactTemp = func(dbPath string) string { return dbPath + ".compact" }

// Compact rewrites the database at dbPath without its free pages and with
// incremental auto-vacuum, so that from then on clearing and pruning give
// space back (Reclaim). Nothing else may have the database open: a server
// or another command using it gives ErrInUse, and nothing can open it while
// this runs.
//
// The new file is written next to the database (it needs free space for the
// data the database holds), checked (integrity, the same rows in every
// table), and only then renamed over the database; on any failure before
// that, the database is left as it was and the error says nothing was
// changed. With keepOld the old file stays as <db>.before-compact.
func Compact(ctx context.Context, dbPath string, keepOld bool) (CompactResult, error) {
	start := time.Now()
	var res CompactResult
	if _, err := os.Stat(dbPath); err != nil {
		return res, err
	}
	lock, err := lockFile(dbPath+".lock", true)
	if errors.Is(err, errLocked) {
		return res, fmt.Errorf("%w: %s is open in another process (a running server?); stop it first", ErrInUse, dbPath)
	}
	if err != nil {
		return res, fmt.Errorf("lock %s: %w", dbPath, err)
	}
	defer lock.Close()
	old := dbPath + ".before-compact"
	if keepOld {
		if _, err := os.Lstat(old); err == nil {
			return res, fmt.Errorf("%s is in the way (an earlier compact kept it): move it away first; nothing was changed", old)
		}
	}
	tmp := compactTemp(dbPath)
	unchanged := func(err error) (CompactResult, error) {
		os.Remove(tmp)
		return res, fmt.Errorf("%w; nothing was changed", err)
	}

	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		return res, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // one connection: the checkpoint and the rewrite see one state
	ms, err := loadMigrations(migrationFS)
	if err != nil {
		return res, err
	}
	if err := setup(ctx, db, dbPath, ms); err != nil {
		return res, err
	}
	mode, err := autoVacuum(ctx, db)
	if err != nil {
		return res, err
	}
	res.Converted = mode != "incremental"
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return res, fmt.Errorf("checkpoint: %w", err)
	}
	res.Before = sizeWithWAL(dbPath)
	counts, err := tableCounts(ctx, db)
	if err != nil {
		return res, err
	}

	os.Remove(tmp) // a file a failed run left behind
	if _, err := db.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		return unchanged(err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return unchanged(fmt.Errorf("write %s: %w", tmp, err))
	}
	if err := checkCopy(ctx, tmp, counts); err != nil {
		return unchanged(fmt.Errorf("check %s: %w", tmp, err))
	}
	if err := sameOwner(dbPath, tmp); err != nil {
		return unchanged(err)
	}
	if err := db.Close(); err != nil {
		return unchanged(err)
	}
	// Closed with an empty WAL (checkpointed above, nothing written since):
	// a WAL left beside the new file would be read as its own.
	if fi, err := os.Stat(dbPath + "-wal"); err == nil && fi.Size() > 0 {
		return unchanged(fmt.Errorf("%s-wal is not empty", dbPath))
	}
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
	if keepOld {
		if err := os.Link(dbPath, old); err != nil {
			return unchanged(fmt.Errorf("keep the old file: %w", err))
		}
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		if keepOld {
			os.Remove(old)
		}
		return unchanged(err)
	}
	res.After = sizeWithWAL(dbPath)
	res.Took = time.Since(start)
	return res, nil
}

func sizeWithWAL(path string) int64 {
	var n int64
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// tableCounts is the number of rows of every table.
func tableCounts(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	out := map[string]int64{}
	for _, n := range names {
		var c int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+n+`"`).Scan(&c); err != nil {
			return nil, err
		}
		out[n] = c
	}
	return out, nil
}

// checkCopy opens the new file read-only: it must be whole, incremental,
// and hold as many rows in every table as the database.
func checkCopy(ctx context.Context, path string, want map[string]int64) error {
	db, err := sql.Open("sqlite", "file:"+uriPath.Replace(path)+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var ok string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&ok); err != nil {
		return err
	}
	if ok != "ok" {
		return fmt.Errorf("integrity check: %s", ok)
	}
	if mode, err := autoVacuum(ctx, db); err != nil || mode != "incremental" {
		return fmt.Errorf("auto_vacuum %q (%v)", mode, err)
	}
	got, err := tableCounts(ctx, db)
	if err != nil {
		return err
	}
	if len(got) != len(want) {
		return fmt.Errorf("%d tables, the database has %d", len(got), len(want))
	}
	for n, c := range want {
		if got[n] != c {
			return fmt.Errorf("table %s has %d rows, the database %d", n, got[n], c)
		}
	}
	return nil
}
