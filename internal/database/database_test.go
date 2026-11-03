package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationsAndConsistentSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Integrity(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec("INSERT INTO settings(key,value) VALUES('sample','{\"value\":1}')"); err != nil {
		t.Fatal(err)
	}
	copy := filepath.Join(dir, "snapshot.db")
	if err := s.Snapshot(ctx, copy); err != nil {
		t.Fatal(err)
	}
	other, err := Open(copy)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var count int
	if err := other.QueryRow("SELECT count(*) FROM settings").Scan(&count); err != nil || count != 1 {
		t.Fatalf("snapshot count=%d err=%v", count, err)
	}
	if info, err := os.Stat(copy); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("snapshot permissions: %v", err)
	}
	if _, err := s.Exec("INSERT INTO sessions VALUES('x','missing','csrf',1)"); err == nil {
		t.Fatal("foreign key was not enforced")
	}
	if err := s.Snapshot(ctx, copy); err == nil {
		t.Fatal("overwrote existing snapshot")
	}
}

func TestRejectFutureSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Exec("PRAGMA user_version=100"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if newer, err := Open(path); err == nil {
		newer.Close()
		t.Fatal("accepted a future schema")
	}
}
