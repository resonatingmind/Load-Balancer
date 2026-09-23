// Command backend is a tiny HTTP server used as an upstream for local
// testing and benchmarking of the load balancer.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":9001", "address to listen on")
	name := flag.String("name", "", "name reported in responses (defaults to addr)")
	delay := flag.Duration("delay", 0, "artificial latency added to each request, e.g. 50ms")
	flag.Parse()
	if *name == "" {
		*name = *addr
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if *delay > 0 {
			select {
			case <-time.After(*delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("X-Backend", *name)
		fmt.Fprintf(w, "hello from %s (path=%s)\n", *name, r.URL.Path)
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("backend %s listening on %s", *name, *addr)
	log.Fatal(srv.ListenAndServe())
}
