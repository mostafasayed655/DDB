package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"ddb/internal/common"
)

type aggregatorServer struct {
	cfg     common.AggregatorConfig
	client  *http.Client
	timeout time.Duration
}

func main() {
	configPath := flag.String("config", "config/aggregator.json", "path to config file")
	flag.Parse()

	var cfg common.AggregatorConfig
	if err := common.LoadJSON(*configPath, &cfg); err != nil {
		log.Fatal(err)
	}
	if cfg.Address == "" {
		cfg.Address = ":8004"
	}
	if cfg.RequestTimeoutMs <= 0 {
		cfg.RequestTimeoutMs = 5000
	}

	srv := &aggregatorServer{
		cfg:     cfg,
		client:  common.NewHTTPClient(time.Duration(cfg.RequestTimeoutMs) * time.Millisecond),
		timeout: time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/query", srv.handleQuery)

	log.Printf("aggregator listening on %s", cfg.Address)
	log.Fatal(http.ListenAndServe(cfg.Address, mux))
}

func (a *aggregatorServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	common.EncodeJSON(w, http.StatusOK, common.GenericResponse{OK: true, Message: "ok"})
}

func (a *aggregatorServer) handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req common.SelectRequest
	if err := common.DecodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	rows, failed := a.queryWorkers(req)
	if len(rows) == 0 && len(failed) == len(a.cfg.Workers) {
		writeError(w, http.StatusBadGateway, "all workers unavailable")
		return
	}

	resp := common.GenericResponse{OK: true, Rows: rows, Count: len(rows)}
	if len(failed) > 0 {
		resp.FailedWorkers = failed
	}
	common.EncodeJSON(w, http.StatusOK, resp)
}

func (a *aggregatorServer) queryWorkers(req common.SelectRequest) ([]map[string]any, []string) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	rows := make([]map[string]any, 0)
	failed := make([]string, 0)

	for _, worker := range a.cfg.Workers {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			var resp common.GenericResponse
			ctx, cancel := context.WithTimeout(context.Background(), a.timeout)
			defer cancel()
			err := common.PostJSON(ctx, a.client, worker.BaseURL+"/select", req, &resp)
			if err != nil || !resp.OK {
				mu.Lock()
				failed = append(failed, worker.Name)
				mu.Unlock()
				return
			}
			if len(resp.Rows) > 0 {
				mu.Lock()
				rows = append(rows, resp.Rows...)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	rows = dedupeRows(rows, req.DedupeKey)
	if req.Limit > 0 && len(rows) > req.Limit {
		rows = rows[:req.Limit]
	}
	return rows, failed
}

func dedupeRows(rows []map[string]any, key string) []map[string]any {
	if key == "" {
		return rows
	}
	seen := make(map[string]bool, len(rows))
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		val, ok := row[key]
		if !ok {
			out = append(out, row)
			continue
		}
		keyValue := fmt.Sprint(val)
		if seen[keyValue] {
			continue
		}
		seen[keyValue] = true
		out = append(out, row)
	}
	return out
}

func writeError(w http.ResponseWriter, status int, msg string) {
	common.EncodeJSON(w, status, common.GenericResponse{OK: false, Message: msg})
}
