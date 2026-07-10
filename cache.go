package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	humane "github.com/sierrasoftworks/humane-errors-go"
)

// versionCache records the versions this service has applied per job variable.
// When a path is configured it persists the record to disk (written
// atomically), so a restart does not forget and re-apply a version Nomad
// auto-reverted after a failed deployment. With an empty path it is purely
// in-memory.
type versionCache struct {
	mu      sync.Mutex
	path    string
	applied map[string]map[string]string // job (namespace/id) -> variable -> value
}

// newVersionCache creates an empty cache. Call Load to populate it from disk.
func newVersionCache(path string) *versionCache {
	return &versionCache{path: path, applied: map[string]map[string]string{}}
}

// Load reads any persisted state from the configured path. A missing file is
// not an error (first run), and it is a no-op when no path is configured.
func (c *versionCache) Load() error {
	if c.path == "" {
		return nil
	}
	data, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return humane.Wrap(err, "could not read the version cache at "+c.path,
			"Check the file is readable, or set -cache-file to a writable path (the bundled job uses a sticky ephemeral disk).")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}

	loaded := map[string]map[string]string{}
	if err := json.Unmarshal(data, &loaded); err != nil {
		return humane.Wrap(err, "could not parse the version cache at "+c.path,
			"The file is corrupt; delete it to start with an empty cache.")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied = loaded
	return nil
}

// get reports the last value this service applied to a job variable.
func (c *versionCache) get(job, name string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	vars, ok := c.applied[job]
	if !ok {
		return "", false
	}
	v, ok := vars[name]
	return v, ok
}

// set records that this service applied value to a job variable and persists
// the cache when a path is configured.
func (c *versionCache) set(job, name, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	vars := c.applied[job]
	if vars == nil {
		vars = map[string]string{}
		c.applied[job] = vars
	}
	vars[name] = value
	return c.saveLocked()
}

// saveLocked atomically writes the cache to disk. The caller must hold c.mu.
func (c *versionCache) saveLocked() error {
	if c.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(c.applied, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return humane.Wrap(err, "could not create the version cache directory "+dir)
	}
	tmp, err := os.CreateTemp(dir, ".applied-*.tmp")
	if err != nil {
		return humane.Wrap(err, "could not create a temporary file for the version cache")
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		os.Remove(tmpName)
		return humane.Wrap(err, "could not persist the version cache to "+c.path)
	}
	return nil
}
