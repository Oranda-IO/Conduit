package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	targetHost string
	domainName string
	ports      *PortTable
	settings   *SettingsStore
	mu         sync.Mutex
	proxies    map[int]*httputil.ReverseProxy
}

func NewServer(targetHost string, ports *PortTable, settings *SettingsStore, domainName string) *Server {
	if settings == nil {
		settings = NewSettingsStore(defaultSettingsFilePath())
		_ = settings.Load()
	}
	if domainName == "" {
		domainName = "conduit.local"
	}
	domainName = strings.ToLower(strings.TrimSpace(domainName))

	return &Server{
		targetHost: targetHost,
		domainName: domainName,
		ports:      ports,
		settings:   settings,
		proxies:    map[int]*httputil.ReverseProxy{},
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ports", s.handlePorts)
	mux.HandleFunc("/apps", s.handleApps)
	mux.HandleFunc("/ui", s.handleUI)
	mux.HandleFunc("/", s.handleProxy)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handlePorts(w http.ResponseWriter, _ *http.Request) {
	ports := s.ports.List()
	writeJSON(w, http.StatusOK, map[string]any{
		"ports":      ports,
		"count":      len(ports),
		"updated_at": s.ports.LastUpdated().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if err := s.settings.Load(); err != nil {
		log.Printf("settings load warning: %v", err)
	}

	target, ok := s.resolveTarget(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if !s.ports.Has(target.Port) {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":         "target internal port is not currently listening",
			"internal_port": target.Port,
			"app_name":      target.Name,
		})
		return
	}

	proxy := s.proxyFor(target.Port)
	originalPath := r.URL.Path
	originalRawPath := r.URL.RawPath

	r.URL.Path = target.UpstreamPath
	r.URL.RawPath = target.UpstreamPath
	r.Header.Set("X-Conduit-Target-Port", strconv.Itoa(target.Port))
	if target.Name != "" {
		r.Header.Set("X-Conduit-Target-Name", target.Name)
	} else {
		r.Header.Del("X-Conduit-Target-Name")
	}

	proxy.ServeHTTP(w, r)

	r.URL.Path = originalPath
	r.URL.RawPath = originalRawPath
}

type routeTarget struct {
	Port         int
	Name         string
	UpstreamPath string
}

func (s *Server) resolveTarget(r *http.Request) (routeTarget, bool) {
	if target, ok := s.resolveTargetHost(r.Host, r.URL.Path); ok {
		return target, true
	}
	return s.resolveTargetPath(r.URL.Path)
}

func (s *Server) resolveTargetHost(hostPort, path string) (routeTarget, bool) {
	host := normalizeHost(hostPort)
	if host == "" || s.domainName == "" || host == s.domainName {
		return routeTarget{}, false
	}

	suffix := "." + s.domainName
	if !strings.HasSuffix(host, suffix) {
		return routeTarget{}, false
	}

	namePart := strings.TrimSuffix(host, suffix)
	namePart = strings.TrimSuffix(namePart, ".")
	name, err := normalizeAppName(namePart)
	if err != nil {
		return routeTarget{}, false
	}

	port, found := s.settings.Lookup(name)
	if !found {
		return routeTarget{}, false
	}

	upstreamPath := path
	if upstreamPath == "" {
		upstreamPath = "/"
	}

	return routeTarget{
		Port:         port,
		Name:         name,
		UpstreamPath: upstreamPath,
	}, true
}

func (s *Server) resolveTargetPath(path string) (routeTarget, bool) {
	firstSegment, upstreamPath, ok := splitRoute(path)
	if !ok {
		return routeTarget{}, false
	}

	if port, err := strconv.Atoi(firstSegment); err == nil {
		if port >= 1 && port <= 65535 {
			return routeTarget{Port: port, UpstreamPath: upstreamPath}, true
		}
		return routeTarget{}, false
	}

	name, err := normalizeAppName(firstSegment)
	if err != nil {
		return routeTarget{}, false
	}

	port, found := s.settings.Lookup(name)
	if !found {
		return routeTarget{}, false
	}

	return routeTarget{Port: port, Name: name, UpstreamPath: upstreamPath}, true
}

func (s *Server) proxyFor(port int) *httputil.ReverseProxy {
	s.mu.Lock()
	defer s.mu.Unlock()

	if p, ok := s.proxies[port]; ok {
		return p
	}

	target := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s:%d", s.targetHost, port),
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = log.New(log.Writer(), "proxy: ", log.LstdFlags)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":   "failed to proxy request",
			"details": err.Error(),
		})
	}

	s.proxies[port] = proxy
	return proxy
}

func parseInternalPortPath(path string) (int, string, bool) {
	firstSegment, upstreamPath, ok := splitRoute(path)
	if !ok {
		return 0, "", false
	}

	port, err := strconv.Atoi(firstSegment)
	if err != nil || port < 1 || port > 65535 {
		return 0, "", false
	}

	return port, upstreamPath, true
}

func splitRoute(path string) (string, string, bool) {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return "", "", false
	}

	parts := strings.SplitN(trimmed, "/", 2)
	firstSegment := strings.TrimSpace(parts[0])
	if firstSegment == "" {
		return "", "", false
	}

	upstreamPath := "/"
	if len(parts) == 2 && parts[1] != "" {
		upstreamPath = "/" + parts[1]
	}

	return firstSegment, upstreamPath, true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type cliConfig struct {
	publicHost   string
	publicPort   int
	targetHost   string
	pollInterval time.Duration
	settingsFile string
	domainName   string
	noHTTP       bool
	noUI         bool
	jsonOutput   bool
}

func parseConfig(args []string, stderr io.Writer) (cliConfig, []string, error) {
	cfg := cliConfig{}
	fs := flag.NewFlagSet("conduit", flag.ContinueOnError)
	fs.SetOutput(stderr)

	fs.StringVar(&cfg.publicHost, "public-host", "0.0.0.0", "Public host to bind")
	fs.IntVar(&cfg.publicPort, "public-port", 9000, "Public port to expose conduit")
	fs.StringVar(&cfg.targetHost, "target-host", "127.0.0.1", "Host for local upstream services")
	fs.DurationVar(&cfg.pollInterval, "poll-interval", 2*time.Second, "How often to rescan local listening ports")
	fs.StringVar(&cfg.settingsFile, "settings-file", defaultSettingsFilePath(), "Path to conduit settings JSON")
	fs.StringVar(&cfg.domainName, "domain-name", "conduit.local", "Domain suffix for host-based app routing")
	fs.BoolVar(&cfg.noHTTP, "no-http", false, "Run without starting the HTTP server")
	fs.BoolVar(&cfg.noUI, "no-ui", false, "Alias for --no-http")
	fs.BoolVar(&cfg.jsonOutput, "json", false, "Use JSON output for CLI commands")

	if err := fs.Parse(args); err != nil {
		return cliConfig{}, nil, err
	}

	if cfg.noUI {
		cfg.noHTTP = true
	}

	if cfg.publicPort < 1 || cfg.publicPort > 65535 {
		return cliConfig{}, nil, errors.New("public-port must be in range 1-65535")
	}
	if cfg.pollInterval < 250*time.Millisecond {
		return cliConfig{}, nil, errors.New("poll-interval must be >= 250ms")
	}

	return cfg, fs.Args(), nil
}

func main() {
	cfg, args, err := parseConfig(os.Args[1:], os.Stderr)
	if err != nil {
		log.Fatal(err)
	}

	if len(args) > 0 {
		if err := runCommand(cfg, args, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}

	if err := runServer(cfg); err != nil {
		log.Fatal(err)
	}
}

func runServer(cfg cliConfig) error {
	var publicHost string
	var publicPort int
	var targetHost string
	var pollInterval time.Duration
	var settingsFile string
	var domainName string

	publicHost = cfg.publicHost
	publicPort = cfg.publicPort
	targetHost = cfg.targetHost
	pollInterval = cfg.pollInterval
	settingsFile = cfg.settingsFile
	domainName = cfg.domainName

	portTable := NewPortTable()
	if err := portTable.Refresh(); err != nil {
		return fmt.Errorf("initial port discovery failed: %w", err)
	}
	go portTable.Watch(pollInterval)

	settings := NewSettingsStore(settingsFile)
	if err := settings.Load(); err != nil {
		log.Printf("settings load warning (%s): %v", settingsFile, err)
	}

	server := NewServer(targetHost, portTable, settings, domainName)
	addr := fmt.Sprintf("%s:%d", publicHost, publicPort)

	log.Printf("conduit listening on http://%s", addr)
	log.Printf("route format: http://<host>:%d/<internal_port>/<optional_path>", publicPort)
	log.Printf("named route format: http://<host>:%d/<app_name>/<optional_path>", publicPort)
	log.Printf("host route format: http://<app>.%s:%d/", domainName, publicPort)
	log.Printf("settings file: %s", settingsFile)
	log.Printf("active local ports: %v", sortedPorts(portTable.snapshot()))

	if cfg.noHTTP {
		log.Printf("http server disabled (--no-http); discovery/watch still running")
		select {}
	}
	log.Printf("dashboard: http://%s/ui", addr)

	if err := http.ListenAndServe(addr, server.routes()); err != nil {
		return fmt.Errorf("server failed: %w", err)
	}
	return nil
}

func runCommand(cfg cliConfig, args []string, out io.Writer) error {
	portTable := NewPortTable()
	if err := portTable.Refresh(); err != nil {
		return fmt.Errorf("port discovery failed: %w", err)
	}

	settings := NewSettingsStore(cfg.settingsFile)
	if err := settings.Load(); err != nil {
		return fmt.Errorf("settings load failed: %w", err)
	}

	switch args[0] {
	case "health":
		return printCommandOutput(out, cfg.jsonOutput, map[string]string{"status": "ok"}, "ok")
	case "ports":
		return runPortsCommand(out, cfg.jsonOutput, portTable)
	case "apps":
		return runAppsCommand(out, cfg.jsonOutput, settings, portTable, args[1:])
	default:
		return fmt.Errorf("unknown command %q (supported: health, ports, apps)", args[0])
	}
}

func runPortsCommand(out io.Writer, jsonOutput bool, portTable *PortTable) error {
	ports := portTable.List()
	if jsonOutput {
		payload := map[string]any{
			"ports":      ports,
			"count":      len(ports),
			"updated_at": portTable.LastUpdated().UTC().Format(time.RFC3339),
		}
		return json.NewEncoder(out).Encode(payload)
	}

	for _, p := range ports {
		fmt.Fprintln(out, p)
	}
	return nil
}

func runAppsCommand(out io.Writer, jsonOutput bool, settings *SettingsStore, portTable *PortTable, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		return runAppsListCommand(out, jsonOutput, settings, portTable)
	}

	switch args[0] {
	case "set":
		if len(args) != 3 {
			return errors.New("usage: conduit apps set <name> <port>")
		}
		port, err := strconv.Atoi(args[2])
		if err != nil {
			return errors.New("port must be an integer")
		}
		name, err := settings.Set(args[1], port)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(out).Encode(map[string]any{
				"ok":     true,
				"action": "set",
				"name":   name,
				"port":   port,
			})
		}
		fmt.Fprintf(out, "set %s -> %d\n", name, port)
		return nil
	case "delete":
		if len(args) != 2 {
			return errors.New("usage: conduit apps delete <name>")
		}
		name, err := settings.Delete(args[1])
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(out).Encode(map[string]any{
				"ok":     true,
				"action": "delete",
				"name":   name,
			})
		}
		fmt.Fprintf(out, "deleted %s\n", name)
		return nil
	default:
		return fmt.Errorf("unknown apps command %q (supported: list, set, delete)", args[0])
	}
}

func runAppsListCommand(out io.Writer, jsonOutput bool, settings *SettingsStore, portTable *PortTable) error {
	snapshot := portTable.snapshot()
	apps := settings.List(snapshot)
	mappedPorts := map[int]struct{}{}
	appRoutes := make([]appRouteInfo, 0, len(apps))
	for _, app := range apps {
		mappedPorts[app.Port] = struct{}{}
		appRoutes = append(appRoutes, appRouteInfo{
			Name:      app.Name,
			Port:      app.Port,
			Running:   app.Running,
			NamedPath: "/" + app.Name + "/",
			PortPath:  fmt.Sprintf("/%d/", app.Port),
		})
	}

	unmapped := make([]int, 0)
	for _, port := range sortedPorts(snapshot) {
		if _, ok := mappedPorts[port]; !ok {
			unmapped = append(unmapped, port)
		}
	}

	payload := appsResponse{
		Apps:         appRoutes,
		Unmapped:     unmapped,
		SettingsFile: settings.Path(),
		UpdatedAt:    portTable.LastUpdated().UTC().Format(time.RFC3339),
	}

	if jsonOutput {
		return json.NewEncoder(out).Encode(payload)
	}

	if len(payload.Apps) == 0 {
		fmt.Fprintln(out, "no app aliases configured")
	} else {
		for _, app := range payload.Apps {
			status := "down"
			if app.Running {
				status = "running"
			}
			fmt.Fprintf(out, "%s -> %d (%s)\n", app.Name, app.Port, status)
		}
	}
	if len(payload.Unmapped) > 0 {
		fmt.Fprintf(out, "unmapped running ports: %v\n", payload.Unmapped)
	}
	return nil
}

func printCommandOutput(out io.Writer, jsonOutput bool, payload any, text string) error {
	if jsonOutput {
		return json.NewEncoder(out).Encode(payload)
	}
	_, err := fmt.Fprintln(out, text)
	return err
}

func normalizeHost(hostPort string) string {
	host := strings.TrimSpace(hostPort)
	if host == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		return strings.ToLower(strings.TrimSuffix(h, "."))
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func defaultSettingsFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".conduit/settings.json"
	}
	return home + "/.conduit/settings.json"
}

func sortedPorts(ports map[int]struct{}) []int {
	out := make([]int, 0, len(ports))
	for p := range ports {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}
