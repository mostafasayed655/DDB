package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"ddb/internal/common"
)

type masterServer struct {
	cfg           common.MasterConfig
	client        *http.Client
	timeout       time.Duration
	rr            uint64
	workerByName  map[string]common.WorkerConfig
}

func main() {
	configPath := flag.String("config", "config/master.json", "path to config file")
	flag.Parse()

	var cfg common.MasterConfig
	if err := common.LoadJSON(*configPath, &cfg); err != nil {
		log.Fatal(err)
	}
	if cfg.Address == "" {
		cfg.Address = ":8000"
	}
	if cfg.RequestTimeoutMs <= 0 {
		cfg.RequestTimeoutMs = 5000
	}

	workerByName := make(map[string]common.WorkerConfig)
	for _, w := range cfg.Workers {
		workerByName[w.Name] = w
	}

	srv := &masterServer{
		cfg:          cfg,
		client:       common.NewHTTPClient(time.Duration(cfg.RequestTimeoutMs) * time.Millisecond),
		timeout:      time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		workerByName: workerByName,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.handleHealth)
	mux.HandleFunc("/table/create", srv.handleFanout("/table/create", func() any { return &common.CreateTableRequest{} }))
	mux.HandleFunc("/table/drop", srv.handleFanout("/table/drop", func() any { return &common.DropTableRequest{} }))
	mux.HandleFunc("/insert", srv.handleInsert)
	mux.HandleFunc("/select", srv.handleSelect)
	mux.HandleFunc("/search", srv.handleSelect)
	mux.HandleFunc("/update", srv.handleFanout("/update", func() any { return &common.UpdateRequest{} }))
	mux.HandleFunc("/delete", srv.handleFanout("/delete", func() any { return &common.DeleteRequest{} }))
	mux.HandleFunc("/db/drop", srv.handleDropDB)
	mux.HandleFunc("/special/summary", srv.handleSpecialSummary)

	log.Printf("master listening on %s", cfg.Address)
	log.Fatal(http.ListenAndServe(cfg.Address, mux))
}

func (m *masterServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	common.EncodeJSON(w, http.StatusOK, common.GenericResponse{OK: true, Message: "ok"})
}

func (m *masterServer) handleInsert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req common.InsertRequest
	if err := common.DecodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if len(m.cfg.Workers) == 0 {
		writeError(w, http.StatusBadGateway, "no workers configured")
		return
	}

	start := m.nextWorkerIndex()
	var primary common.WorkerConfig
	var primaryResp common.GenericResponse

	for i := 0; i < len(m.cfg.Workers); i++ {
		candidate := m.cfg.Workers[(start+i)%len(m.cfg.Workers)]
		primaryResp = common.GenericResponse{}
		err := m.postToWorker(candidate, "/insert", req, &primaryResp)
		if err == nil && primaryResp.OK {
			primary = candidate
			break
		}
	}

	if primary.Name == "" {
		writeError(w, http.StatusBadGateway, "all workers unavailable for insert")
		return
	}

	failed := m.replicateExcept(primary.Name, "/insert", req)
	resp := common.GenericResponse{OK: true, Message: fmt.Sprintf("inserted via %s", primary.Name)}
	if len(failed) > 0 {
		resp.FailedReplicas = failed
	}
	common.EncodeJSON(w, http.StatusOK, resp)
}

func (m *masterServer) handleSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req common.SelectRequest
	if err := common.DecodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if m.cfg.AggregatorURL != "" {
		var aggResp common.GenericResponse
		ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
		defer cancel()
		err := common.PostJSON(ctx, m.client, m.cfg.AggregatorURL+"/query", req, &aggResp)
		if err == nil && aggResp.OK {
			common.EncodeJSON(w, http.StatusOK, aggResp)
			return
		}
	}

	rows, failed := m.queryWorkers(req)
	if len(rows) == 0 && len(failed) == len(m.cfg.Workers) {
		writeError(w, http.StatusBadGateway, "all workers unavailable")
		return
	}

	resp := common.GenericResponse{OK: true, Rows: rows, Count: len(rows)}
	if len(failed) > 0 {
		resp.FailedWorkers = failed
	}
	common.EncodeJSON(w, http.StatusOK, resp)
}

func (m *masterServer) handleDropDB(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	failed, okCount := m.fanout("/db/drop", map[string]any{})
	if okCount == 0 {
		writeError(w, http.StatusBadGateway, "all workers unavailable")
		return
	}

	resp := common.GenericResponse{OK: true, Message: "db dropped"}
	if len(failed) > 0 {
		resp.FailedReplicas = failed
	}
	common.EncodeJSON(w, http.StatusOK, resp)
}

func (m *masterServer) handleSpecialSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req common.SummaryRequest
	if err := common.DecodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	results := make([]common.SummaryResponse, 0)
	failed := make([]string, 0)
	var total int

	for _, name := range m.cfg.SpecialWorkers {
		worker, ok := m.workerByName[name]
		if !ok {
			failed = append(failed, name)
			continue
		}
		var resp common.SummaryResponse
		ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
		err := common.PostJSON(ctx, m.client, worker.BaseURL+"/special/summary", req, &resp)
		cancel()
		if err != nil || !resp.OK {
			failed = append(failed, worker.Name)
			continue
		}
		results = append(results, resp)
		total += resp.Count
	}

	common.EncodeJSON(w, http.StatusOK, common.SummaryAggregateResponse{OK: true, Results: results, TotalCount: total, FailedWorkers: failed})
}

func (m *masterServer) handleFanout(path string, newReq func() any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}

		payload := newReq()
		if err := common.DecodeJSON(r, payload); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json")
			return
		}

		failed, okCount := m.fanout(path, payload)
		if okCount == 0 {
			writeError(w, http.StatusBadGateway, "all workers unavailable")
			return
		}

		resp := common.GenericResponse{OK: true, Message: "fanout complete"}
		if len(failed) > 0 {
			resp.FailedReplicas = failed
		}
		common.EncodeJSON(w, http.StatusOK, resp)
	}
}

func (m *masterServer) fanout(path string, payload any) ([]string, int) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := make([]string, 0)
	okCount := 0

	for _, worker := range m.cfg.Workers {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			var resp common.GenericResponse
			err := m.postToWorker(worker, path, payload, &resp)
			if err != nil || !resp.OK {
				mu.Lock()
				failed = append(failed, worker.Name)
				mu.Unlock()
				return
			}
			mu.Lock()
			okCount++
			mu.Unlock()
		}()
	}

	wg.Wait()
	return failed, okCount
}

func (m *masterServer) replicateExcept(primaryName, path string, payload any) []string {
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := make([]string, 0)

	for _, worker := range m.cfg.Workers {
		if worker.Name == primaryName {
			continue
		}
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			var resp common.GenericResponse
			err := m.postToWorker(worker, path, payload, &resp)
			if err != nil || !resp.OK {
				mu.Lock()
				failed = append(failed, worker.Name)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	return failed
}

func (m *masterServer) queryWorkers(req common.SelectRequest) ([]map[string]any, []string) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	rows := make([]map[string]any, 0)
	failed := make([]string, 0)

	for _, worker := range m.cfg.Workers {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			var resp common.GenericResponse
			err := m.postToWorker(worker, "/select", req, &resp)
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

func (m *masterServer) postToWorker(worker common.WorkerConfig, path string, payload any, out *common.GenericResponse) error {
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()
	return common.PostJSON(ctx, m.client, worker.BaseURL+path, payload, out)
}

func (m *masterServer) nextWorkerIndex() int {
	if len(m.cfg.Workers) == 0 {
		return 0
	}
	return int(atomic.AddUint64(&m.rr, 1)-1) % len(m.cfg.Workers)
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
