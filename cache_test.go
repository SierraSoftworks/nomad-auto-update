package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVersionCacheInMemory(t *testing.T) {
	c := newVersionCache("")
	if err := c.set("default/grey", "grey_version", "v2.2.0"); err != nil {
		t.Fatal(err)
	}
	if v, ok := c.get("default/grey", "grey_version"); !ok || v != "v2.2.0" {
		t.Fatalf("get = %q, %v", v, ok)
	}
	if _, ok := c.get("default/grey", "other"); ok {
		t.Fatal("unknown variable should be absent")
	}
	if _, ok := c.get("default/other", "grey_version"); ok {
		t.Fatal("unknown job should be absent")
	}
}

func TestVersionCachePersistsAndReloads(t *testing.T) {
	// A nested path also exercises directory creation.
	path := filepath.Join(t.TempDir(), "state", "applied-versions.json")

	c := newVersionCache(path)
	if err := c.set("default/grey", "grey_version", "v2.2.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache file was not written: %v", err)
	}

	reloaded := newVersionCache(path)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if v, ok := reloaded.get("default/grey", "grey_version"); !ok || v != "v2.2.0" {
		t.Fatalf("reloaded get = %q, %v", v, ok)
	}
}

func TestVersionCacheLoadMissingFileIsClean(t *testing.T) {
	c := newVersionCache(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err := c.Load(); err != nil {
		t.Fatalf("a missing cache file must not be an error: %v", err)
	}
}

func TestVersionCacheLoadCorruptErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(path, []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newVersionCache(path)
	if err := c.Load(); err == nil {
		t.Fatal("a corrupt cache file should surface an error")
	}
}
