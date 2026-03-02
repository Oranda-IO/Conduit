package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var appNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

type settingsFile struct {
	Apps map[string]int `json:"apps"`
}

type AppMapping struct {
	Name    string `json:"name"`
	Port    int    `json:"port"`
	Running bool   `json:"running"`
}

type SettingsStore struct {
	path string
	mu   sync.RWMutex
	apps map[string]int
}

func NewSettingsStore(path string) *SettingsStore {
	return &SettingsStore{path: path, apps: map[string]int{}}
}

func (s *SettingsStore) Path() string {
	return s.path
}

func (s *SettingsStore) Load() error {
	content, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.mu.Lock()
			s.apps = map[string]int{}
			s.mu.Unlock()
			return nil
		}
		return err
	}

	var parsed settingsFile
	if err := json.Unmarshal(content, &parsed); err != nil {
		return fmt.Errorf("invalid settings json: %w", err)
	}

	normalized := map[string]int{}
	for name, port := range parsed.Apps {
		normName, err := normalizeAppName(name)
		if err != nil {
			continue
		}
		if port < 1 || port > 65535 {
			continue
		}
		normalized[normName] = port
	}

	s.mu.Lock()
	s.apps = normalized
	s.mu.Unlock()
	return nil
}

func (s *SettingsStore) Save() error {
	s.mu.RLock()
	copyApps := make(map[string]int, len(s.apps))
	for name, port := range s.apps {
		copyApps[name] = port
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(settingsFile{Apps: copyApps}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}

	return os.WriteFile(s.path, data, 0o644)
}

func (s *SettingsStore) Lookup(name string) (int, bool) {
	normalized, err := normalizeAppName(name)
	if err != nil {
		return 0, false
	}

	s.mu.RLock()
	port, ok := s.apps[normalized]
	s.mu.RUnlock()
	return port, ok
}

func (s *SettingsStore) Set(name string, port int) (string, error) {
	normalized, err := normalizeAppName(name)
	if err != nil {
		return "", err
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("port must be in range 1-65535")
	}

	s.mu.Lock()
	s.apps[normalized] = port
	s.mu.Unlock()

	if err := s.Save(); err != nil {
		return "", err
	}

	return normalized, nil
}

func (s *SettingsStore) Delete(name string) (string, error) {
	normalized, err := normalizeAppName(name)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	delete(s.apps, normalized)
	s.mu.Unlock()

	if err := s.Save(); err != nil {
		return "", err
	}

	return normalized, nil
}

func (s *SettingsStore) List(activePorts map[int]struct{}) []AppMapping {
	s.mu.RLock()
	out := make([]AppMapping, 0, len(s.apps))
	for name, port := range s.apps {
		_, running := activePorts[port]
		out = append(out, AppMapping{Name: name, Port: port, Running: running})
	}
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func normalizeAppName(name string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	if !appNamePattern.MatchString(normalized) {
		return "", fmt.Errorf("app name must match %s", appNamePattern.String())
	}
	return normalized, nil
}
