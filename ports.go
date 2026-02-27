package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tcpListenState = "0A"
)

var procNetPaths = []string{"/proc/net/tcp", "/proc/net/tcp6"}

type PortTable struct {
	mu          sync.RWMutex
	ports       map[int]struct{}
	lastUpdated time.Time
}

func NewPortTable() *PortTable {
	return &PortTable{ports: map[int]struct{}{}}
}

func (t *PortTable) Refresh() error {
	ports, err := discoverListeningTCPPorts(procNetPaths)
	if err != nil {
		return err
	}

	t.mu.Lock()
	t.ports = ports
	t.lastUpdated = time.Now()
	t.mu.Unlock()

	return nil
}

func (t *PortTable) Watch(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		if err := t.Refresh(); err != nil {
			fmt.Fprintf(os.Stderr, "port watch refresh failed: %v\n", err)
		}
	}
}

func (t *PortTable) Has(port int) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.ports[port]
	return ok
}

func (t *PortTable) List() []int {
	return sortedPorts(t.snapshot())
}

func (t *PortTable) snapshot() map[int]struct{} {
	t.mu.RLock()
	defer t.mu.RUnlock()

	clone := make(map[int]struct{}, len(t.ports))
	for p := range t.ports {
		clone[p] = struct{}{}
	}
	return clone
}

func (t *PortTable) LastUpdated() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastUpdated
}

func discoverListeningTCPPorts(paths []string) (map[int]struct{}, error) {
	ports := map[int]struct{}{}
	var hadSuccess bool
	var errs []string

	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
			continue
		}

		parsed, err := parseProcNetTCP(f)
		_ = f.Close()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
			continue
		}

		hadSuccess = true
		for p := range parsed {
			ports[p] = struct{}{}
		}
	}

	if !hadSuccess {
		return nil, fmt.Errorf("failed to read proc tcp tables: %s", strings.Join(errs, "; "))
	}

	return ports, nil
}

func parseProcNetTCP(file *os.File) (map[int]struct{}, error) {
	scanner := bufio.NewScanner(file)
	ports := map[int]struct{}{}
	isHeader := true

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if isHeader {
			isHeader = false
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}

		if fields[3] != tcpListenState {
			continue
		}

		addrParts := strings.Split(fields[1], ":")
		if len(addrParts) != 2 {
			continue
		}

		port, err := strconv.ParseInt(addrParts[1], 16, 32)
		if err != nil {
			continue
		}
		if port > 0 {
			ports[int(port)] = struct{}{}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return ports, nil
}
