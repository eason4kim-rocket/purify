package ledger

import (
	"path/filepath"
	"sync"
)

// openCoordinator serializes database initialization for one physical path
// while allowing unrelated ledgers to start independently. SQLite still owns
// cross-process coordination through its configured busy timeout.
type openCoordinator struct {
	mu      sync.Mutex
	entries map[string]*openCoordinatorEntry
}

type openCoordinatorEntry struct {
	mu   sync.Mutex
	refs int
}

func (c *openCoordinator) lock(path string) func() {
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]*openCoordinatorEntry)
	}
	entry := c.entries[path]
	if entry == nil {
		entry = &openCoordinatorEntry{}
		c.entries[path] = entry
	}
	entry.refs++
	c.mu.Unlock()

	entry.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(c.entries, path)
			}
			entry.mu.Unlock()
			c.mu.Unlock()
		})
	}
}

// databaseIdentityPath resolves aliases in the existing data directory so
// callers using relative paths or directory symlinks share one coordinator
// entry. The database itself may not exist on first startup.
func databaseIdentityPath(dbPath string) string {
	absolutePath, err := filepath.Abs(dbPath)
	if err != nil {
		return filepath.Clean(dbPath)
	}
	if resolvedPath, err := filepath.EvalSymlinks(absolutePath); err == nil {
		return filepath.Clean(resolvedPath)
	}
	if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(absolutePath)); err == nil {
		return filepath.Join(resolvedDir, filepath.Base(absolutePath))
	}
	return filepath.Clean(absolutePath)
}
