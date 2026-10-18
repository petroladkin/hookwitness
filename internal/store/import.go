package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ImportItem is one endpoint of a setup being imported.
type ImportItem struct {
	Path string
	// Replace lets the import replace the configuration of an endpoint
	// already at Path (its requests, counter and on/off state stay);
	// without it a taken path fails the whole import.
	Replace bool
	// Config gives the configuration to store from the endpoint's current
	// one (exists false for a new endpoint); the caller validates it.
	Config func(old string, exists bool) (string, error)
}

// ImportResult says what an import did to one endpoint.
type ImportResult struct {
	ID      int64
	Path    string
	Created bool
	Changed bool // a new endpoint, or a configuration that differs from before
}

// ImportEndpoints creates or replaces endpoints in one transaction: either
// every item is applied or none.
func (s *Store) ImportEndpoints(ctx context.Context, items []ImportItem) ([]ImportResult, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := millis(time.Now().UTC())
	out := make([]ImportResult, 0, len(items))
	for _, it := range items {
		var id int64
		var old string
		err := tx.QueryRowContext(ctx, `SELECT id, config FROM endpoints WHERE path = ?`, it.Path).Scan(&id, &old)
		exists := err == nil
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, err
		case !it.Replace:
			return nil, fmt.Errorf("%s: %w", it.Path, ErrDuplicatePath)
		}
		cfg, err := it.Config(old, exists)
		if err != nil {
			return nil, err
		}
		r := ImportResult{ID: id, Path: it.Path, Created: !exists, Changed: !exists || cfg != old}
		switch {
		case !exists:
			res, err := tx.ExecContext(ctx, `INSERT INTO endpoints (path, created_at, config) VALUES (?, ?, ?)`, it.Path, now, cfg)
			if err != nil {
				if isUnique(err) {
					return nil, fmt.Errorf("%s: %w", it.Path, ErrDuplicatePath)
				}
				return nil, fmt.Errorf("create endpoint: %w", err)
			}
			if r.ID, err = res.LastInsertId(); err != nil {
				return nil, err
			}
		case r.Changed:
			if _, err := tx.ExecContext(ctx, `UPDATE endpoints SET config = ? WHERE id = ?`, cfg, id); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
