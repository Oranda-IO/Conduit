package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strconv"
)

func main() {
	var port int
	flag.IntVar(&port, "port", 3000, "listen port")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"service": "demo-echo",
			"path":    r.URL.Path,
			"query":   r.URL.RawQuery,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("demo-echo says hello\n"))
	})

	addr := ":" + strconv.Itoa(port)
	log.Printf("demo-echo listening on http://127.0.0.1%s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
