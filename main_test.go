package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

	conduit := NewServer(host, portTable, newTestSettingsStore(t))
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

func TestHTTPProxyByNameEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"path":               r.URL.Path,
			"query":              r.URL.RawQuery,
			"target_port_header": r.Header.Get("X-Conduit-Target-Port"),
			"target_name_header": r.Header.Get("X-Conduit-Target-Name"),
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

	settings := newTestSettingsStore(t)
	if _, err := settings.Set("myapp", port); err != nil {
		t.Fatalf("settings set: %v", err)
	}

	portTable := NewPortTable()
	portTable.mu.Lock()
	portTable.ports[port] = struct{}{}
	portTable.lastUpdated = time.Now()
	portTable.mu.Unlock()

	conduit := NewServer(host, portTable, settings)
	public := httptest.NewServer(conduit.routes())
	defer public.Close()

	resp, err := http.Get(public.URL + "/myapp/hello/world?x=1")
	if err != nil {
		t.Fatalf("GET via named route: %v", err)
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
	if payload["target_name_header"] != "myapp" {
		t.Fatalf("target_name_header=%q want=myapp", payload["target_name_header"])
	}
}

func TestAppsAPIPostAndGet(t *testing.T) {
	portTable := NewPortTable()
	portTable.mu.Lock()
	portTable.ports[3000] = struct{}{}
	portTable.lastUpdated = time.Now()
	portTable.mu.Unlock()

	settings := newTestSettingsStore(t)
	conduit := NewServer("127.0.0.1", portTable, settings)
	public := httptest.NewServer(conduit.routes())
	defer public.Close()

	body := []byte(`{"action":"set","name":"api","port":3000}`)
	resp, err := http.Post(public.URL+"/apps", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /apps: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("POST /apps status=%d body=%s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	resp, err = http.Get(public.URL + "/apps")
	if err != nil {
		t.Fatalf("GET /apps: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /apps status=%d body=%s", resp.StatusCode, string(b))
	}

	var payload appsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode /apps: %v", err)
	}
	if len(payload.Apps) != 1 {
		t.Fatalf("apps count=%d want=1", len(payload.Apps))
	}
	if payload.Apps[0].Name != "api" || payload.Apps[0].Port != 3000 || !payload.Apps[0].Running {
		t.Fatalf("unexpected app payload: %+v", payload.Apps[0])
	}
}

func TestProxyRejectsInactivePort(t *testing.T) {
	portTable := NewPortTable()
	portTable.mu.Lock()
	portTable.lastUpdated = time.Now()
	portTable.mu.Unlock()

	conduit := NewServer("127.0.0.1", portTable, newTestSettingsStore(t))
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

func TestParseConfigNoUIImpliesNoHTTP(t *testing.T) {
	cfg, args, err := parseConfig([]string{"--no-ui", "apps", "list"}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig error: %v", err)
	}
	if !cfg.noUI {
		t.Fatalf("expected noUI=true")
	}
	if !cfg.noHTTP {
		t.Fatalf("expected noHTTP=true when noUI is set")
	}
	if len(args) != 2 || args[0] != "apps" || args[1] != "list" {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestRunAppsCommandSetListDeleteJSON(t *testing.T) {
	settings := newTestSettingsStore(t)
	portTable := NewPortTable()
	portTable.mu.Lock()
	portTable.ports[3000] = struct{}{}
	portTable.lastUpdated = time.Now()
	portTable.mu.Unlock()

	setOut := &bytes.Buffer{}
	if err := runAppsCommand(setOut, true, settings, portTable, []string{"set", "api", "3000"}); err != nil {
		t.Fatalf("runAppsCommand set: %v", err)
	}

	var setPayload map[string]any
	if err := json.Unmarshal(setOut.Bytes(), &setPayload); err != nil {
		t.Fatalf("unmarshal set payload: %v", err)
	}
	if setPayload["action"] != "set" || setPayload["name"] != "api" {
		t.Fatalf("unexpected set payload: %#v", setPayload)
	}

	listOut := &bytes.Buffer{}
	if err := runAppsCommand(listOut, true, settings, portTable, []string{"list"}); err != nil {
		t.Fatalf("runAppsCommand list: %v", err)
	}

	var listPayload appsResponse
	if err := json.Unmarshal(listOut.Bytes(), &listPayload); err != nil {
		t.Fatalf("unmarshal list payload: %v", err)
	}
	if len(listPayload.Apps) != 1 {
		t.Fatalf("apps count=%d want=1", len(listPayload.Apps))
	}
	if listPayload.Apps[0].Name != "api" || listPayload.Apps[0].Port != 3000 || !listPayload.Apps[0].Running {
		t.Fatalf("unexpected app payload: %+v", listPayload.Apps[0])
	}

	deleteOut := &bytes.Buffer{}
	if err := runAppsCommand(deleteOut, true, settings, portTable, []string{"delete", "api"}); err != nil {
		t.Fatalf("runAppsCommand delete: %v", err)
	}

	if _, ok := settings.Lookup("api"); ok {
		t.Fatalf("expected api mapping to be deleted")
	}
}

func TestParseConfigNoHTTPFlag(t *testing.T) {
	cfg, args, err := parseConfig([]string{"--no-http"}, io.Discard)
	if err != nil {
		t.Fatalf("parseConfig error: %v", err)
	}
	if !cfg.noHTTP {
		t.Fatalf("expected noHTTP=true")
	}
	if len(args) != 0 {
		t.Fatalf("unexpected args: %#v", args)
	}
}

func TestRunCommandHealthTextAndJSON(t *testing.T) {
	cfg := cliConfig{settingsFile: filepath.Join(t.TempDir(), "settings.json")}

	textOut := &bytes.Buffer{}
	if err := runCommand(cfg, []string{"health"}, textOut); err != nil {
		t.Fatalf("runCommand health text: %v", err)
	}
	if strings.TrimSpace(textOut.String()) != "ok" {
		t.Fatalf("unexpected health text output: %q", textOut.String())
	}

	cfg.jsonOutput = true
	jsonOut := &bytes.Buffer{}
	if err := runCommand(cfg, []string{"health"}, jsonOut); err != nil {
		t.Fatalf("runCommand health json: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(jsonOut.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal health json: %v", err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("unexpected health payload: %#v", payload)
	}
}

func TestRunCommandPortsTextAndJSON(t *testing.T) {
	restore := useProcNetFixture(t, procNetFixtureWithPorts(3000, 5173))
	defer restore()

	cfg := cliConfig{settingsFile: filepath.Join(t.TempDir(), "settings.json")}

	textOut := &bytes.Buffer{}
	if err := runCommand(cfg, []string{"ports"}, textOut); err != nil {
		t.Fatalf("runCommand ports text: %v", err)
	}
	lines := strings.Fields(strings.TrimSpace(textOut.String()))
	if len(lines) != 2 || lines[0] != "3000" || lines[1] != "5173" {
		t.Fatalf("unexpected ports text output: %q", textOut.String())
	}

	cfg.jsonOutput = true
	jsonOut := &bytes.Buffer{}
	if err := runCommand(cfg, []string{"ports"}, jsonOut); err != nil {
		t.Fatalf("runCommand ports json: %v", err)
	}
	var payload struct {
		Ports []int `json:"ports"`
		Count int   `json:"count"`
	}
	if err := json.Unmarshal(jsonOut.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal ports json: %v", err)
	}
	if payload.Count != 2 || len(payload.Ports) != 2 || payload.Ports[0] != 3000 || payload.Ports[1] != 5173 {
		t.Fatalf("unexpected ports json payload: %+v", payload)
	}
}

func TestRunCommandAppsListParityWithHTTP(t *testing.T) {
	restore := useProcNetFixture(t, procNetFixtureWithPorts(3000, 5432))
	defer restore()

	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	settings := NewSettingsStore(settingsPath)
	if err := settings.Load(); err != nil {
		t.Fatalf("settings load: %v", err)
	}
	if _, err := settings.Set("api", 3000); err != nil {
		t.Fatalf("settings set: %v", err)
	}

	cfg := cliConfig{settingsFile: settingsPath, jsonOutput: true}
	cliOut := &bytes.Buffer{}
	if err := runCommand(cfg, []string{"apps", "list"}, cliOut); err != nil {
		t.Fatalf("runCommand apps list: %v", err)
	}
	var cliPayload appsResponse
	if err := json.Unmarshal(cliOut.Bytes(), &cliPayload); err != nil {
		t.Fatalf("unmarshal cli apps list: %v", err)
	}

	portTable := NewPortTable()
	if err := portTable.Refresh(); err != nil {
		t.Fatalf("port refresh: %v", err)
	}
	server := NewServer("127.0.0.1", portTable, settings)
	httpServer := httptest.NewServer(server.routes())
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL + "/apps")
	if err != nil {
		t.Fatalf("GET /apps: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /apps status=%d body=%s", resp.StatusCode, string(body))
	}
	var httpPayload appsResponse
	if err := json.NewDecoder(resp.Body).Decode(&httpPayload); err != nil {
		t.Fatalf("decode /apps payload: %v", err)
	}

	if len(cliPayload.Apps) != len(httpPayload.Apps) {
		t.Fatalf("apps len mismatch cli=%d http=%d", len(cliPayload.Apps), len(httpPayload.Apps))
	}
	if len(cliPayload.Apps) != 1 {
		t.Fatalf("expected one app in payloads; got cli=%d", len(cliPayload.Apps))
	}
	if cliPayload.Apps[0].Name != httpPayload.Apps[0].Name || cliPayload.Apps[0].Port != httpPayload.Apps[0].Port || cliPayload.Apps[0].Running != httpPayload.Apps[0].Running {
		t.Fatalf("app mismatch cli=%+v http=%+v", cliPayload.Apps[0], httpPayload.Apps[0])
	}
	if len(cliPayload.Unmapped) != len(httpPayload.Unmapped) || cliPayload.Unmapped[0] != httpPayload.Unmapped[0] {
		t.Fatalf("unmapped mismatch cli=%v http=%v", cliPayload.Unmapped, httpPayload.Unmapped)
	}
}

func TestRunCommandUnknownCommand(t *testing.T) {
	cfg := cliConfig{settingsFile: filepath.Join(t.TempDir(), "settings.json")}
	err := runCommand(cfg, []string{"wat"}, io.Discard)
	if err == nil {
		t.Fatalf("expected unknown command error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func useProcNetFixture(t *testing.T, content string) func() {
	t.Helper()
	f, err := os.CreateTemp("", "conduit-procnet")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatalf("WriteString: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	original := procNetPaths
	procNetPaths = []string{f.Name()}

	return func() {
		procNetPaths = original
		_ = os.Remove(f.Name())
	}
}

func procNetFixtureWithPorts(ports ...int) string {
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i, port := range ports {
		b.WriteString(fmt.Sprintf("   %d: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000   100        0 000000 1 0000000000000000 100 0 0 10 0\n", i, port))
	}
	return b.String()
}

func newTestSettingsStore(t *testing.T) *SettingsStore {
	t.Helper()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "settings.json")
	store := NewSettingsStore(path)
	if err := store.Load(); err != nil {
		t.Fatalf("load settings: %v", err)
	}
	return store
}
