package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	targetHost string
	ports      *PortTable
	mu         sync.Mutex
	proxies    map[int]*httputil.ReverseProxy
}

func NewServer(targetHost string, ports *PortTable) *Server {
	return &Server{
		targetHost: targetHost,
		ports:      ports,
		proxies:    map[int]*httputil.ReverseProxy{},
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ports", s.handlePorts)
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
	port, upstreamPath, ok := parseInternalPortPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if !s.ports.Has(port) {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":         "target internal port is not currently listening",
			"internal_port": port,
		})
		return
	}

	proxy := s.proxyFor(port)
	originalPath := r.URL.Path
	originalRawPath := r.URL.RawPath

	r.URL.Path = upstreamPath
	r.URL.RawPath = upstreamPath
	r.Header.Set("X-Conduit-Target-Port", strconv.Itoa(port))

	proxy.ServeHTTP(w, r)

	r.URL.Path = originalPath
	r.URL.RawPath = originalRawPath
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
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return 0, "", false
	}

	parts := strings.SplitN(trimmed, "/", 2)
	port, err := strconv.Atoi(parts[0])
	if err != nil || port < 1 || port > 65535 {
		return 0, "", false
	}

	upstreamPath := "/"
	if len(parts) == 2 && parts[1] != "" {
		upstreamPath = "/" + parts[1]
	}

	return port, upstreamPath, true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func main() {
	var publicHost string
	var publicPort int
	var targetHost string
	var pollInterval time.Duration

	flag.StringVar(&publicHost, "public-host", "0.0.0.0", "Public host to bind")
	flag.IntVar(&publicPort, "public-port", 9000, "Public port to expose conduit")
	flag.StringVar(&targetHost, "target-host", "127.0.0.1", "Host for local upstream services")
	flag.DurationVar(&pollInterval, "poll-interval", 2*time.Second, "How often to rescan local listening ports")
	flag.Parse()

	if publicPort < 1 || publicPort > 65535 {
		log.Fatal("public-port must be in range 1-65535")
	}
	if pollInterval < 250*time.Millisecond {
		log.Fatal("poll-interval must be >= 250ms")
	}

	portTable := NewPortTable()
	if err := portTable.Refresh(); err != nil {
		log.Fatalf("initial port discovery failed: %v", err)
	}
	go portTable.Watch(pollInterval)

	server := NewServer(targetHost, portTable)
	addr := fmt.Sprintf("%s:%d", publicHost, publicPort)

	log.Printf("conduit listening on http://%s", addr)
	log.Printf("route format: http://<host>:%d/<internal_port>/<optional_path>", publicPort)
	log.Printf("active local ports: %v", sortedPorts(portTable.snapshot()))

	if err := http.ListenAndServe(addr, server.routes()); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func sortedPorts(ports map[int]struct{}) []int {
	out := make([]int, 0, len(ports))
	for p := range ports {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}
