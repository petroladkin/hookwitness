package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AutoVacuum is the database's auto_vacuum mode: "none", "full" or
// "incremental". Databases created by this version are incremental; older
// ones are none until compacted (Compact).
func (s *Store) AutoVacuum(ctx context.Context) (string, error) {
	return autoVacuum(ctx, s.db)
}

func autoVacuum(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	var mode int
	if err := q.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return "", fmt.Errorf("auto_vacuum: %w", err)
	}
	switch mode {
	case 1:
		return "full", nil
	case 2:
		return "incremental", nil
	}
	return "none", nil
}

// ReclaimStep is the most pages one step of Reclaim frees: a step holds the
// writer, so catching waits at most that long.
var ReclaimStep = 1024

// reclaimHook, when set (by tests), is told the pages each step freed.
var reclaimHook func(freed int64)

// Reclaim gives the database's free pages back to the file system, when at
// least minBytes are free (0: any), in steps of at most ReclaimStep pages,
// each a write of its own so that catching goes on between them. It returns
// the bytes given back. A database without incremental auto-vacuum is left
// as it is (Compact converts it). The file shrinks at the next checkpoint.
func (s *Store) Reclaim(ctx context.Context, minBytes int64) (int64, error) {
	mode, err := s.AutoVacuum(ctx)
	if err != nil || mode != "incremental" {
		return 0, err
	}
	var pageSize int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, fmt.Errorf("page size: %w", err)
	}
	free, err := s.freePages(ctx)
	if err != nil || free == 0 || free*pageSize < minBytes {
		return 0, err
	}
	var total int64
	for free > 0 && ctx.Err() == nil {
		freed, err := s.reclaimStep(ctx)
		if err != nil {
			return total * pageSize, err
		}
		if reclaimHook != nil {
			reclaimHook(freed)
		}
		if freed == 0 {
			break
		}
		total += freed
		free -= freed
	}
	// Let the shrink reach the file now rather than at the next automatic
	// checkpoint; a busy reader only postpones it.
	s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	return total * pageSize, ctx.Err()
}

func (s *Store) freePages(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&n); err != nil {
		return 0, fmt.Errorf("free pages: %w", err)
	}
	return n, nil
}

// reclaimStep frees at most ReclaimStep pages under the writer's lock and
// reports how many it freed.
func (s *Store) reclaimStep(ctx context.Context) (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var before, after int64
	if err := conn.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&before); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, ReclaimStep)); err != nil {
		return 0, fmt.Errorf("incremental vacuum: %w", err)
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&after); err != nil {
		return 0, err
	}
	return before - after, nil
}

// ReclaimFrom is how much free space the housekeeping waits for before it
// gives it back (space freed by pruning; clearing gives its own back at once).
const ReclaimFrom = 8 << 20

// Housekeep gives free space back every interval (Reclaim from
// ReclaimFrom) until ctx ends; errors go to report.
func (s *Store) Housekeep(ctx context.Context, interval time.Duration, report func(freed int64, err error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			freed, err := s.Reclaim(ctx, ReclaimFrom)
			if (freed > 0 || err != nil) && ctx.Err() == nil {
				report(freed, err)
			}
		}
	}
}
