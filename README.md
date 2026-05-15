# DDB (Distributed DB)

## Architecture
- Master (Go): API gateway. Routes client requests, chooses a primary worker for inserts, and replicates writes.
- Workers (Go, Node.js, C#): store MySQL data locally and serve CRUD requests.
- Aggregator (Go): map/reduce node that fans out read queries and merges results.

## Features
- Dynamic table creation with custom columns
- CRUD operations via HTTP/JSON
- Synchronous replication (fan-out writes)
- Basic fault tolerance (skips unreachable workers)
- Polyglot workers with a special summary endpoint

## Ports (default)
- Master: 8000
- Worker (Go): 8001
- Worker (Node.js): 8002
- Worker (C#): 8003
- Aggregator: 8004

## Prerequisites
- Go 1.22+
- Node.js 18+
- .NET 8 SDK
- MySQL Server 8+

## Setup
### Go
```
go mod tidy
```

### Node.js worker
```
cd worker-node
npm install
```

### MySQL config
Edit each worker config file and set credentials:
- [config/worker-go.json](config/worker-go.json)
- [config/worker-node.json](config/worker-node.json)
- [config/worker-cs.json](config/worker-cs.json)

### C# worker
```
dotnet restore .\worker-cs\Worker.csproj
```

## Run (local)
Start each node in its own terminal:

```
go run .\cmd\worker-go -config .\config\worker-go.json
```
```
cd worker-node
npm start
```
```
dotnet run --project .\worker-cs\Worker.csproj -- --config .\config\worker-cs.json
```
```
go run .\cmd\aggregator -config .\config\aggregator.json
```
```
go run .\cmd\master -config .\config\master.json
```

## Client GUI
Run the GUI client and open it in your browser:
```
go run .\cmd\client-web -address :8088 -master http://localhost:8000 -static .\client-web
```
Open http://localhost:8088 and send requests to the master.

## Example requests
### Create table
```
curl -X POST http://localhost:8000/table/create -H "Content-Type: application/json" -d "{\"table\":\"users\",\"columns\":[{\"name\":\"id\",\"type\":\"INTEGER\"},{\"name\":\"name\",\"type\":\"TEXT\"}],\"primaryKey\":\"id\"}"
```

### Insert
```
curl -X POST http://localhost:8000/insert -H "Content-Type: application/json" -d "{\"table\":\"users\",\"values\":{\"id\":1,\"name\":\"Mona\"}}"
```

### Select
```
curl -X POST http://localhost:8000/select -H "Content-Type: application/json" -d "{\"table\":\"users\",\"where\":\"id=1\",\"limit\":10,\"dedupeKey\":\"id\"}"
```

### Update
```
curl -X POST http://localhost:8000/update -H "Content-Type: application/json" -d "{\"table\":\"users\",\"set\":{\"name\":\"Sara\"},\"where\":\"id=1\"}"
```

### Delete
```
curl -X POST http://localhost:8000/delete -H "Content-Type: application/json" -d "{\"table\":\"users\",\"where\":\"id=1\"}"
```

### Drop table
```
curl -X POST http://localhost:8000/table/drop -H "Content-Type: application/json" -d "{\"table\":\"users\"}"
```

### Drop database (master only)
```
curl -X POST http://localhost:8000/db/drop
```

### Special summary (polyglot bonus)
```
curl -X POST http://localhost:8000/special/summary -H "Content-Type: application/json" -d "{\"table\":\"users\"}"
```

## Notes
- The `where` field is raw SQL (trusted input only).
- Use `dedupeKey` on read queries to remove duplicates after aggregation.
- If the master goes down, you can still query workers directly or spin up a new master using the same config.
- Update IPs/ports in `config/*.json` when moving from localhost to real devices.
- MySQL database names are validated as simple identifiers (letters, numbers, underscores).

## Report
See `report/REPORT.md` for the final report outline.
