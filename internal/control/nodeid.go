package control

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// EnsureNodeID returns the persistent node identity. An explicit override
// (REMOTECTL_NODE_ID) wins; otherwise state/slaver/node-id is read, or a new
// UUID is generated and stored with 0600 permissions.
func EnsureNodeID(stateDir, override string) (string, error) {
	if override = strings.TrimSpace(override); override != "" {
		return override, nil
	}
	path := filepath.Join(stateDir, "slaver", "node-id")
	data, err := os.ReadFile(path)
	if err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id, nil
		}
		return "", fmt.Errorf("node id file %s is empty", path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read node id: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}
	id := uuid.NewString()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write node id: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("store node id: %w", err)
	}
	return id, nil
}
