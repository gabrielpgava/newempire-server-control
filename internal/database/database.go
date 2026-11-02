package database

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

const SchemaVersion = 1

type Store struct {
	*sql.DB
	Path string
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(FULL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db, path}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	conn, err := s.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK") // A successful COMMIT makes this a no-op.
	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > SchemaVersion {
		return errors.New("database schema is newer than this binary; refusing downgrade")
	}
	for n := version + 1; n <= SchemaVersion; n++ {
		entries, err := migrations.ReadDir("migrations")
		if err != nil {
			return err
		}
		found := false
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), fmt.Sprintf("%03d_", n)) {
				continue
			}
			body, err := migrations.ReadFile("migrations/" + entry.Name())
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %d: %w", n, err)
			}
			found = true
		}
		if !found {
			return fmt.Errorf("missing migration %d", n)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", n)); err != nil {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

func (s *Store) Integrity(ctx context.Context) error {
	var result string
	if err := s.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return errors.New("SQLite integrity check failed")
	}
	rows, err := s.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("SQLite foreign key check failed")
	}
	return rows.Err()
}

// Snapshot uses SQLite's consistent online snapshot, including committed WAL pages.
func (s *Store) Snapshot(ctx context.Context, destination string) error {
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	f.Close()
	if err := os.Remove(destination); err != nil {
		return err
	}
	_, err = s.ExecContext(ctx, "VACUUM INTO ?", destination)
	if err != nil {
		return fmt.Errorf("snapshot database: %w", err)
	}
	return os.Chmod(destination, 0600)
}
