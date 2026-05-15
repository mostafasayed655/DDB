package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	_ "github.com/go-sql-driver/mysql"

	"ddb/internal/common"
)

var identRe = regexp.MustCompile("^[A-Za-z_][A-Za-z0-9_]*$")
var typeRe = regexp.MustCompile("^[A-Za-z0-9_(),\\s]+$")

type workerState struct {
	cfg common.DBWorkerConfig
	db  *sql.DB
	mu  sync.RWMutex
}

func main() {
	configPath := flag.String("config", "config/worker-go.json", "path to config file")
	flag.Parse()

	var cfg common.DBWorkerConfig
	if err := common.LoadJSON(*configPath, &cfg); err != nil {
		log.Fatal(err)
	}
	// TODO: set mysql credentials in config/worker-go.json.
	if cfg.Address == "" {
		cfg.Address = ":8001"
	}
	if cfg.Name == "" {
		cfg.Name = "worker-go"
	}
	if cfg.MySQL.Host == "" {
		cfg.MySQL.Host = "127.0.0.1"
	}
	if cfg.MySQL.Port == 0 {
		cfg.MySQL.Port = 3306
	}
	if cfg.MySQL.Params == "" {
		cfg.MySQL.Params = "parseTime=true&multiStatements=true"
	}
	if cfg.MySQL.User == "" {
		log.Fatal("mysql.user is required")
	}
	if cfg.MySQL.Database == "" {
		log.Fatal("mysql.database is required")
	}
	if !validIdent(cfg.MySQL.Database) {
		log.Fatal("mysql.database must be a simple identifier")
	}

	db, err := openMySQL(cfg.MySQL)
	if err != nil {
		log.Fatal(err)
	}

	state := &workerState{cfg: cfg, db: db}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", state.handleHealth)
	mux.HandleFunc("/table/create", state.handleCreateTable)
	mux.HandleFunc("/table/drop", state.handleDropTable)
	mux.HandleFunc("/insert", state.handleInsert)
	mux.HandleFunc("/select", state.handleSelect)
	mux.HandleFunc("/update", state.handleUpdate)
	mux.HandleFunc("/delete", state.handleDelete)
	mux.HandleFunc("/db/drop", state.handleDropDB)

	log.Printf("worker %s listening on %s", cfg.Name, cfg.Address)
	log.Fatal(http.ListenAndServe(cfg.Address, mux))
}

func (s *workerState) handleHealth(w http.ResponseWriter, r *http.Request) {
	common.EncodeJSON(w, http.StatusOK, common.GenericResponse{OK: true, Message: "ok"})
}

func (s *workerState) handleCreateTable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req common.CreateTableRequest
	if err := common.DecodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if !validIdent(req.Table) {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}
	if len(req.Columns) == 0 {
		writeError(w, http.StatusBadRequest, "columns required")
		return
	}

	cols := make([]string, 0, len(req.Columns)+1)
	for _, col := range req.Columns {
		if !validIdent(col.Name) {
			writeError(w, http.StatusBadRequest, "invalid column name")
			return
		}
		colType := strings.TrimSpace(col.Type)
		if colType == "" || !typeRe.MatchString(colType) {
			writeError(w, http.StatusBadRequest, "invalid column type")
			return
		}
		cols = append(cols, fmt.Sprintf("%s %s", col.Name, colType))
	}
	if req.PrimaryKey != "" {
		if !validIdent(req.PrimaryKey) {
			writeError(w, http.StatusBadRequest, "invalid primary key")
			return
		}
		cols = append(cols, fmt.Sprintf("PRIMARY KEY (%s)", req.PrimaryKey))
	}

	stmt := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)", req.Table, strings.Join(cols, ", "))

	if err := s.withDB(func(db *sql.DB) error {
		_, err := db.Exec(stmt)
		return err
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	common.EncodeJSON(w, http.StatusOK, common.GenericResponse{OK: true, Message: "table created"})
}

func (s *workerState) handleDropTable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req common.DropTableRequest
	if err := common.DecodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !validIdent(req.Table) {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}

	stmt := fmt.Sprintf("DROP TABLE IF EXISTS %s", req.Table)
	if err := s.withDB(func(db *sql.DB) error {
		_, err := db.Exec(stmt)
		return err
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	common.EncodeJSON(w, http.StatusOK, common.GenericResponse{OK: true, Message: "table dropped"})
}

func (s *workerState) handleInsert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req common.InsertRequest
	if err := common.DecodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !validIdent(req.Table) {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}
	if len(req.Values) == 0 {
		writeError(w, http.StatusBadRequest, "values required")
		return
	}

	cols := make([]string, 0, len(req.Values))
	for k := range req.Values {
		if !validIdent(k) {
			writeError(w, http.StatusBadRequest, "invalid column name")
			return
		}
		cols = append(cols, k)
	}
	sort.Strings(cols)

	placeholders := make([]string, 0, len(cols))
	args := make([]any, 0, len(cols))
	for _, col := range cols {
		placeholders = append(placeholders, "?")
		args = append(args, req.Values[col])
	}

	stmt := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", req.Table, strings.Join(cols, ", "), strings.Join(placeholders, ", "))
	if err := s.withDB(func(db *sql.DB) error {
		_, err := db.Exec(stmt, args...)
		return err
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	common.EncodeJSON(w, http.StatusOK, common.GenericResponse{OK: true, Message: "inserted"})
}

func (s *workerState) handleSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req common.SelectRequest
	if err := common.DecodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !validIdent(req.Table) {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}

	stmt := fmt.Sprintf("SELECT * FROM %s", req.Table)
	if strings.TrimSpace(req.Where) != "" {
		stmt += " WHERE " + req.Where
	}
	if req.Limit > 0 {
		stmt += fmt.Sprintf(" LIMIT %d", req.Limit)
	}

	var rowsData []map[string]any
	if err := s.withDB(func(db *sql.DB) error {
		rows, err := db.Query(stmt)
		if err != nil {
			return err
		}
		defer rows.Close()
		rowsData, err = rowsToMaps(rows)
		return err
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	common.EncodeJSON(w, http.StatusOK, common.GenericResponse{OK: true, Rows: rowsData, Count: len(rowsData)})
}

func (s *workerState) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req common.UpdateRequest
	if err := common.DecodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !validIdent(req.Table) {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}
	if len(req.Set) == 0 {
		writeError(w, http.StatusBadRequest, "set values required")
		return
	}

	cols := make([]string, 0, len(req.Set))
	for k := range req.Set {
		if !validIdent(k) {
			writeError(w, http.StatusBadRequest, "invalid column name")
			return
		}
		cols = append(cols, k)
	}
	sort.Strings(cols)

	setParts := make([]string, 0, len(cols))
	args := make([]any, 0, len(cols))
	for _, col := range cols {
		setParts = append(setParts, fmt.Sprintf("%s = ?", col))
		args = append(args, req.Set[col])
	}

	stmt := fmt.Sprintf("UPDATE %s SET %s", req.Table, strings.Join(setParts, ", "))
	if strings.TrimSpace(req.Where) != "" {
		stmt += " WHERE " + req.Where
	}

	var rowsAffected int64
	if err := s.withDB(func(db *sql.DB) error {
		res, err := db.Exec(stmt, args...)
		if err != nil {
			return err
		}
		rowsAffected, _ = res.RowsAffected()
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	common.EncodeJSON(w, http.StatusOK, common.GenericResponse{OK: true, Message: fmt.Sprintf("updated %d rows", rowsAffected)})
}

func (s *workerState) handleDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req common.DeleteRequest
	if err := common.DecodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !validIdent(req.Table) {
		writeError(w, http.StatusBadRequest, "invalid table name")
		return
	}

	stmt := fmt.Sprintf("DELETE FROM %s", req.Table)
	if strings.TrimSpace(req.Where) != "" {
		stmt += " WHERE " + req.Where
	}

	var rowsAffected int64
	if err := s.withDB(func(db *sql.DB) error {
		res, err := db.Exec(stmt)
		if err != nil {
			return err
		}
		rowsAffected, _ = res.RowsAffected()
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	common.EncodeJSON(w, http.StatusOK, common.GenericResponse{OK: true, Message: fmt.Sprintf("deleted %d rows", rowsAffected)})
}

func (s *workerState) handleDropDB(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if err := s.resetDatabase(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	common.EncodeJSON(w, http.StatusOK, common.GenericResponse{OK: true, Message: "db dropped"})
}

func (s *workerState) resetDatabase() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}

	if err := dropDatabase(s.cfg.MySQL); err != nil {
		return err
	}

	db, err := openMySQL(s.cfg.MySQL)
	if err != nil {
		return err
	}
	s.db = db
	return nil
}

func openMySQL(cfg common.MySQLConfig) (*sql.DB, error) {
	if err := validateMySQL(cfg); err != nil {
		return nil, err
	}
	if err := ensureDatabase(cfg); err != nil {
		return nil, err
	}

	dsn, err := buildDSN(cfg, true)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

func ensureDatabase(cfg common.MySQLConfig) error {
	dsn, err := buildDSN(cfg, false)
	if err != nil {
		return err
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return err
	}

	stmt := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", cfg.Database)
	_, err = db.Exec(stmt)
	return err
}

func dropDatabase(cfg common.MySQLConfig) error {
	dsn, err := buildDSN(cfg, false)
	if err != nil {
		return err
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return err
	}

	stmt := fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", cfg.Database)
	_, err = db.Exec(stmt)
	return err
}

func buildDSN(cfg common.MySQLConfig, includeDB bool) (string, error) {
	if cfg.User == "" {
		return "", errors.New("mysql.user is required")
	}

	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.Port
	if port == 0 {
		port = 3306
	}
	params := strings.TrimSpace(cfg.Params)
	if params == "" {
		params = "parseTime=true&multiStatements=true"
	}

	dbName := ""
	if includeDB {
		if cfg.Database == "" {
			return "", errors.New("mysql.database is required")
		}
		dbName = "/" + cfg.Database
	}

	return fmt.Sprintf("%s:%s@tcp(%s:%d)%s?%s", cfg.User, cfg.Password, host, port, dbName, params), nil
}

func validateMySQL(cfg common.MySQLConfig) error {
	if cfg.User == "" {
		return errors.New("mysql.user is required")
	}
	if cfg.Database == "" {
		return errors.New("mysql.database is required")
	}
	if !validIdent(cfg.Database) {
		return errors.New("mysql.database must be a simple identifier")
	}
	return nil
}

func (s *workerState) withDB(fn func(*sql.DB) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return errors.New("db not initialized")
	}
	return fn(s.db)
}

func rowsToMaps(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(cols))
		valuePtrs := make([]any, len(cols))
		for i := range values {
			valuePtrs[i] = &values[i]
		}
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, err
		}

		row := make(map[string]any, len(cols))
		for i, col := range cols {
			val := values[i]
			if b, ok := val.([]byte); ok {
				row[col] = string(b)
			} else {
				row[col] = val
			}
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func validIdent(s string) bool {
	return identRe.MatchString(s)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	common.EncodeJSON(w, status, common.GenericResponse{OK: false, Message: msg})
}
