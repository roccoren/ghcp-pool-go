package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type HomeRegistry struct {
	root      string
	indexPath string
	index     map[string]string
	mu        sync.Mutex
}

func NewHomeRegistry(root string) *HomeRegistry {
	if root == "" {
		root = "runtime-home"
	}
	h := &HomeRegistry{
		root:      root,
		indexPath: filepath.Join(root, "homes.json"),
		index:     map[string]string{},
	}
	h.load()
	return h
}

func (h *HomeRegistry) load() {
	data, err := os.ReadFile(h.indexPath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &h.index)
}

func (h *HomeRegistry) save() error {
	if err := os.MkdirAll(h.root, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(h.index, "", "  ")
	if err != nil {
		return err
	}
	tmp := h.indexPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.indexPath)
}

func (h *HomeRegistry) Resolve(accountID, configured string) (string, error) {
	if configured != "" {
		abs, err := filepath.Abs(configured)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", err
		}
		return abs, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	assigned := h.index[accountID]
	if assigned == "" {
		assigned = filepath.Join(h.root, newID("home"))
		h.index[accountID] = assigned
		if err := h.save(); err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(assigned)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", err
	}
	return abs, nil
}

func (h *HomeRegistry) Get(accountID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.index[accountID]
}

func (h *HomeRegistry) Forget(accountID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.index[accountID]; ok {
		delete(h.index, accountID)
		_ = h.save()
	}
}

func homeHasCredential(home string) bool {
	if home == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(home, "config.json"))
	return err == nil
}
