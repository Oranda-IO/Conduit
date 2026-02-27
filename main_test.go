package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestParseInternalPortPath(t *testing.T) {
	tests := []struct {
		path         string
		wantPort     int
		wantUpstream string
		wantOK       bool
	}{
		{path: "/8080", wantPort: 8080, wantUpstream: "/", wantOK: true},
		{path: "/8080/", wantPort: 8080, wantUpstream: "/", wantOK: true},
		{path: "/8080/api/v1", wantPort: 8080, wantUpstream: "/api/v1", wantOK: true},
		{path: "/", wantOK: false},
		{path: "/abc", wantOK: false},
		{path: "/0", wantOK: false},
		{path: "/70000", wantOK: false},
	}

	for _, tt := range tests {
		port, upstream, ok := parseInternalPortPath(tt.path)
		if ok != tt.wantOK {
			t.Fatalf("path=%q ok=%v want=%v", tt.path, ok, tt.wantOK)
		}
		if !ok {
			continue
		}
		if port != tt.wantPort {
			t.Fatalf("path=%q port=%d want=%d", tt.path, port, tt.wantPort)
		}
		if upstream != tt.wantUpstream {
			t.Fatalf("path=%q upstream=%q want=%q", tt.path, upstream, tt.wantUpstream)
		}
	}
}

func TestParseProcNetTCP(t *testing.T) {
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000   100        0 000000 1 0000000000000000 100 0 0 10 0
   1: 00000000:1538 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 000000 1 0000000000000000 100 0 0 10 0
   2: 0100007F:0050 00000000:0000 01 00000000:00000000 00:00000000 00000000   100        0 000000 1 0000000000000000 100 0 0 10 0
`

	f, err := os.CreateTemp("", "conduit-proc")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(f.Name())

	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("Seek: %v", err)
	}

	ports, err := parseProcNetTCP(f)
	_ = f.Close()
	if err != nil {
		t.Fatalf("parseProcNetTCP: %v", err)
	}

	if _, ok := ports[8080]; !ok {
		t.Fatalf("expected 8080 in parsed ports")
	}
	if _, ok := ports[5432]; !ok {
		t.Fatalf("expected 5432 in parsed ports")
	}
	if _, ok := ports[80]; ok {
		t.Fatalf("did not expect 80 because state was not LISTEN")
	}
}

func TestHTTPProxyEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"path":               r.URL.Path,
			"query":              r.URL.RawQuery,
			"target_port_header": r.Header.Get("X-Conduit-Target-Port"),
		})
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil {
		t.Fatalf("LookupPort: %v", err)
	}

	portTable := NewPortTable()
	portTable.mu.Lock()
	portTable.ports[port] = struct{}{}
	portTable.lastUpdated = time.Now()
	portTable.mu.Unlock()

	conduit := NewServer(host, portTable)
	public := httptest.NewServer(conduit.routes())
	defer public.Close()

	resp, err := http.Get(fmt.Sprintf("%s/%d/hello/world?x=1", public.URL, port))
	if err != nil {
		t.Fatalf("GET via conduit: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
	}

	var payload map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	if payload["path"] != "/hello/world" {
		t.Fatalf("path=%q want=/hello/world", payload["path"])
	}
	if payload["query"] != "x=1" {
		t.Fatalf("query=%q want=x=1", payload["query"])
	}
	if payload["target_port_header"] != portText {
		t.Fatalf("target_port_header=%q want=%q", payload["target_port_header"], portText)
	}
}

func TestProxyRejectsInactivePort(t *testing.T) {
	portTable := NewPortTable()
	portTable.mu.Lock()
	portTable.lastUpdated = time.Now()
	portTable.mu.Unlock()

	conduit := NewServer("127.0.0.1", portTable)
	public := httptest.NewServer(conduit.routes())
	defer public.Close()

	resp, err := http.Get(public.URL + "/6553/ping")
	if err != nil {
		t.Fatalf("GET via conduit: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, string(body))
	}
}
