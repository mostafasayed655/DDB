package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type forwardRequest struct {
	Path   string          `json:"path"`
	Method string          `json:"method"`
	Body   json.RawMessage `json:"body"`
}

type configResponse struct {
	Master string `json:"master"`
}

func main() {
	address := flag.String("address", ":8088", "client UI listen address")
	master := flag.String("master", "http://localhost:8000", "master base URL")
	staticDir := flag.String("static", "client-web", "static directory")
	flag.Parse()

	masterURL, err := url.Parse(*master)
	if err != nil || masterURL.Scheme == "" || masterURL.Host == "" {
		log.Fatal("invalid master URL")
	}

	if _, err := os.Stat(*staticDir); err != nil {
		log.Fatalf("static dir not found: %v", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(*staticDir)))
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, configResponse{Master: masterURL.String()})
	})
	mux.HandleFunc("/api/forward", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "message": "method not allowed"})
			return
		}

		var req forwardRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "invalid json"})
			return
		}

		path := strings.TrimSpace(req.Path)
		if path == "" || !strings.HasPrefix(path, "/") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "path must start with /"})
			return
		}

		method := strings.ToUpper(strings.TrimSpace(req.Method))
		if method == "" {
			method = http.MethodPost
		}
		if method != http.MethodPost && method != http.MethodGet {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "method must be GET or POST"})
			return
		}

		target := masterURL.ResolveReference(&url.URL{Path: path})
		var body io.Reader
		if method == http.MethodPost {
			trimmed := bytes.TrimSpace(req.Body)
			if len(trimmed) == 0 || string(trimmed) == "null" {
				body = bytes.NewReader([]byte("{}"))
			} else {
				body = bytes.NewReader(req.Body)
			}
		}

		upstream, err := http.NewRequestWithContext(r.Context(), method, target.String(), body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		if method == http.MethodPost {
			upstream.Header.Set("Content-Type", "application/json")
		}

		resp, err := client.Do(upstream)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "message": err.Error()})
			return
		}
		defer resp.Body.Close()

		contentType := resp.Header.Get("Content-Type")
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})

	log.Printf("client UI listening on %s", *address)
	log.Fatal(http.ListenAndServe(*address, mux))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
