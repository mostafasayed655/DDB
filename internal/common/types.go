package common

type WorkerConfig struct {
	Name    string `json:"name"`
	BaseURL string `json:"baseUrl"`
}

type MasterConfig struct {
	Address         string         `json:"address"`
	Workers         []WorkerConfig `json:"workers"`
	AggregatorURL   string         `json:"aggregatorUrl"`
	RequestTimeoutMs int           `json:"requestTimeoutMs"`
	SpecialWorkers  []string       `json:"specialWorkers"`
}

type AggregatorConfig struct {
	Address         string         `json:"address"`
	Workers         []WorkerConfig `json:"workers"`
	RequestTimeoutMs int           `json:"requestTimeoutMs"`
}

// MySQLConfig holds connection settings for a worker database.
type MySQLConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
	Params   string `json:"params"`
}

type DBWorkerConfig struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	// MySQL contains per-worker DB settings.
	MySQL   MySQLConfig `json:"mysql"`
}

type ColumnDef struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type CreateTableRequest struct {
	Table      string      `json:"table"`
	Columns    []ColumnDef `json:"columns"`
	PrimaryKey string      `json:"primaryKey,omitempty"`
}

type DropTableRequest struct {
	Table string `json:"table"`
}

type InsertRequest struct {
	Table  string         `json:"table"`
	Values map[string]any `json:"values"`
}

type SelectRequest struct {
	Table     string `json:"table"`
	Where     string `json:"where,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	DedupeKey string `json:"dedupeKey,omitempty"`
}

type UpdateRequest struct {
	Table string         `json:"table"`
	Set   map[string]any `json:"set"`
	Where string         `json:"where,omitempty"`
}

type DeleteRequest struct {
	Table string `json:"table"`
	Where string `json:"where,omitempty"`
}

type SummaryRequest struct {
	Table string `json:"table"`
}

type SummaryResponse struct {
	OK      bool   `json:"ok"`
	Worker  string `json:"worker"`
	Count   int    `json:"count"`
	Message string `json:"message,omitempty"`
}

type SummaryAggregateResponse struct {
	OK            bool              `json:"ok"`
	Results       []SummaryResponse `json:"results"`
	TotalCount    int               `json:"totalCount"`
	FailedWorkers []string          `json:"failedWorkers,omitempty"`
}

type GenericResponse struct {
	OK             bool              `json:"ok"`
	Message        string            `json:"message,omitempty"`
	Rows           []map[string]any  `json:"rows,omitempty"`
	Count          int               `json:"count,omitempty"`
	FailedReplicas []string          `json:"failedReplicas,omitempty"`
	FailedWorkers  []string          `json:"failedWorkers,omitempty"`
}
