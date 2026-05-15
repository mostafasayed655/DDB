using System.Text.Json;
using System.Text.Json.Serialization;
using System.Text.RegularExpressions;
using MySqlConnector;



var configPath = GetConfigPath(args);
var cfg = LoadConfig(configPath);

cfg.Name = string.IsNullOrWhiteSpace(cfg.Name) ? "worker-cs" : cfg.Name;
cfg.Address = string.IsNullOrWhiteSpace(cfg.Address) ? ":8003" : cfg.Address;

cfg.MySql.Host = string.IsNullOrWhiteSpace(cfg.MySql.Host) ? "127.0.0.1" : cfg.MySql.Host;
cfg.MySql.Port = cfg.MySql.Port == 0 ? 3306 : cfg.MySql.Port;

if (string.IsNullOrWhiteSpace(cfg.MySql.User))
{
    throw new InvalidOperationException("mysql.user is required");
}

if (string.IsNullOrWhiteSpace(cfg.MySql.Database))
{
    throw new InvalidOperationException("mysql.database is required");
}

if (!ProgramRegex.IdentRegex.IsMatch(cfg.MySql.Database))
{
    throw new InvalidOperationException("mysql.database must be a simple identifier");
}

var serverConnectionString = BuildConnectionString(cfg.MySql, includeDatabase: false);
var dbConnectionString = BuildConnectionString(cfg.MySql, includeDatabase: true);
await EnsureDatabaseAsync(serverConnectionString, cfg.MySql.Database);

var builder = WebApplication.CreateBuilder(args);
builder.Services.ConfigureHttpJsonOptions(options =>
{
    options.SerializerOptions.PropertyNamingPolicy = JsonNamingPolicy.CamelCase;
    options.SerializerOptions.DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull;
});

var app = builder.Build();
app.Urls.Add(ResolveListenUrl(cfg.Address));

var jsonOptions = new JsonSerializerOptions
{
    PropertyNameCaseInsensitive = true,
};

app.MapGet("/health", () => Results.Json(new { ok = true, message = "ok" }));

app.MapPost("/table/create", async (HttpRequest request) =>
{
    var req = await request.ReadFromJsonAsync<CreateTableRequest>(jsonOptions);
    if (req == null)
    {
        return Results.Json(new { ok = false, message = "invalid json" }, statusCode: 400);
    }

    if (!IsValidIdent(req.Table))
    {
        return Results.Json(new { ok = false, message = "invalid table name" }, statusCode: 400);
    }

    if (req.Columns == null || req.Columns.Count == 0)
    {
        return Results.Json(new { ok = false, message = "columns required" }, statusCode: 400);
    }

    var defs = new List<string>();
    foreach (var col in req.Columns)
    {
        if (!IsValidIdent(col.Name))
        {
            return Results.Json(new { ok = false, message = "invalid column name" }, statusCode: 400);
        }
        var colType = col.Type?.Trim();
        if (string.IsNullOrWhiteSpace(colType) || !ProgramRegex.TypeRegex.IsMatch(colType))
        {
            return Results.Json(new { ok = false, message = "invalid column type" }, statusCode: 400);
        }
        defs.Add($"{col.Name} {colType}");
    }

    if (!string.IsNullOrWhiteSpace(req.PrimaryKey))
    {
        if (!IsValidIdent(req.PrimaryKey))
        {
            return Results.Json(new { ok = false, message = "invalid primary key" }, statusCode: 400);
        }
        defs.Add($"PRIMARY KEY ({req.PrimaryKey})");
    }

    var sql = $"CREATE TABLE IF NOT EXISTS {req.Table} ({string.Join(", ", defs)})";
    try
    {
        await ExecuteAsync(dbConnectionString, sql, Array.Empty<MySqlParameter>());
        return Results.Json(new { ok = true, message = "table created" });
    }
    catch (Exception ex)
    {
        return Results.Json(new { ok = false, message = ex.Message }, statusCode: 500);
    }
});

app.MapPost("/table/drop", async (HttpRequest request) =>
{
    var req = await request.ReadFromJsonAsync<DropTableRequest>(jsonOptions);
    if (req == null)
    {
        return Results.Json(new { ok = false, message = "invalid json" }, statusCode: 400);
    }

    if (!IsValidIdent(req.Table))
    {
        return Results.Json(new { ok = false, message = "invalid table name" }, statusCode: 400);
    }

    var sql = $"DROP TABLE IF EXISTS {req.Table}";
    try
    {
        await ExecuteAsync(dbConnectionString, sql, Array.Empty<MySqlParameter>());
        return Results.Json(new { ok = true, message = "table dropped" });
    }
    catch (Exception ex)
    {
        return Results.Json(new { ok = false, message = ex.Message }, statusCode: 500);
    }
});

app.MapPost("/insert", async (HttpRequest request) =>
{
    var req = await request.ReadFromJsonAsync<InsertRequest>(jsonOptions);
    if (req == null)
    {
        return Results.Json(new { ok = false, message = "invalid json" }, statusCode: 400);
    }

    if (!IsValidIdent(req.Table))
    {
        return Results.Json(new { ok = false, message = "invalid table name" }, statusCode: 400);
    }

    if (req.Values == null || req.Values.Count == 0)
    {
        return Results.Json(new { ok = false, message = "values required" }, statusCode: 400);
    }

    var cols = req.Values.Keys.OrderBy(key => key).ToList();
    foreach (var col in cols)
    {
        if (!IsValidIdent(col))
        {
            return Results.Json(new { ok = false, message = "invalid column name" }, statusCode: 400);
        }
    }

    var placeholders = cols.Select((_, idx) => $"@p{idx}").ToList();
    var parameters = new List<MySqlParameter>();
    for (var i = 0; i < cols.Count; i++)
    {
        var value = req.Values[cols[i]];
        parameters.Add(new MySqlParameter($"@p{i}", ConvertValue(value)));
    }

    var sql = $"INSERT INTO {req.Table} ({string.Join(", ", cols)}) VALUES ({string.Join(", ", placeholders)})";
    try
    {
        await ExecuteAsync(dbConnectionString, sql, parameters);
        return Results.Json(new { ok = true, message = "inserted" });
    }
    catch (Exception ex)
    {
        return Results.Json(new { ok = false, message = ex.Message }, statusCode: 500);
    }
});

app.MapPost("/select", async (HttpRequest request) =>
{
    var req = await request.ReadFromJsonAsync<SelectRequest>(jsonOptions);
    if (req == null)
    {
        return Results.Json(new { ok = false, message = "invalid json" }, statusCode: 400);
    }

    if (!IsValidIdent(req.Table))
    {
        return Results.Json(new { ok = false, message = "invalid table name" }, statusCode: 400);
    }

    var sql = $"SELECT * FROM {req.Table}";
    if (!string.IsNullOrWhiteSpace(req.Where))
    {
        sql += " WHERE " + req.Where;
    }
    if (req.Limit.HasValue && req.Limit.Value > 0)
    {
        sql += $" LIMIT {req.Limit.Value}";
    }

    try
    {
        var rows = await QueryAsync(dbConnectionString, sql, Array.Empty<MySqlParameter>());
        return Results.Json(new { ok = true, rows, count = rows.Count });
    }
    catch (Exception ex)
    {
        return Results.Json(new { ok = false, message = ex.Message }, statusCode: 500);
    }
});

app.MapPost("/update", async (HttpRequest request) =>
{
    var req = await request.ReadFromJsonAsync<UpdateRequest>(jsonOptions);
    if (req == null)
    {
        return Results.Json(new { ok = false, message = "invalid json" }, statusCode: 400);
    }

    if (!IsValidIdent(req.Table))
    {
        return Results.Json(new { ok = false, message = "invalid table name" }, statusCode: 400);
    }

    if (req.Set == null || req.Set.Count == 0)
    {
        return Results.Json(new { ok = false, message = "set values required" }, statusCode: 400);
    }

    var cols = req.Set.Keys.OrderBy(key => key).ToList();
    foreach (var col in cols)
    {
        if (!IsValidIdent(col))
        {
            return Results.Json(new { ok = false, message = "invalid column name" }, statusCode: 400);
        }
    }

    var setClause = cols.Select((col, idx) => $"{col} = @p{idx}").ToList();
    var parameters = new List<MySqlParameter>();
    for (var i = 0; i < cols.Count; i++)
    {
        var value = req.Set[cols[i]];
        parameters.Add(new MySqlParameter($"@p{i}", ConvertValue(value)));
    }

    var sql = $"UPDATE {req.Table} SET {string.Join(", ", setClause)}";
    if (!string.IsNullOrWhiteSpace(req.Where))
    {
        sql += " WHERE " + req.Where;
    }

    try
    {
        var rowsAffected = await ExecuteAsync(dbConnectionString, sql, parameters);
        return Results.Json(new { ok = true, message = $"updated {rowsAffected} rows" });
    }
    catch (Exception ex)
    {
        return Results.Json(new { ok = false, message = ex.Message }, statusCode: 500);
    }
});

app.MapPost("/delete", async (HttpRequest request) =>
{
    var req = await request.ReadFromJsonAsync<DeleteRequest>(jsonOptions);
    if (req == null)
    {
        return Results.Json(new { ok = false, message = "invalid json" }, statusCode: 400);
    }

    if (!IsValidIdent(req.Table))
    {
        return Results.Json(new { ok = false, message = "invalid table name" }, statusCode: 400);
    }

    var sql = $"DELETE FROM {req.Table}";
    if (!string.IsNullOrWhiteSpace(req.Where))
    {
        sql += " WHERE " + req.Where;
    }

    try
    {
        var rowsAffected = await ExecuteAsync(dbConnectionString, sql, Array.Empty<MySqlParameter>());
        return Results.Json(new { ok = true, message = $"deleted {rowsAffected} rows" });
    }
    catch (Exception ex)
    {
        return Results.Json(new { ok = false, message = ex.Message }, statusCode: 500);
    }
});

app.MapPost("/db/drop", async () =>
{
    try
    {
        await DropAndRecreateDatabaseAsync(serverConnectionString, cfg.MySql.Database);
        return Results.Json(new { ok = true, message = "db dropped" });
    }
    catch (Exception ex)
    {
        return Results.Json(new { ok = false, message = ex.Message }, statusCode: 500);
    }
});

app.MapPost("/special/summary", async (HttpRequest request) =>
{
    var req = await request.ReadFromJsonAsync<SummaryRequest>(jsonOptions);
    if (req == null)
    {
        return Results.Json(new { ok = false, message = "invalid json" }, statusCode: 400);
    }

    if (!IsValidIdent(req.Table))
    {
        return Results.Json(new { ok = false, message = "invalid table name" }, statusCode: 400);
    }

    var sql = $"SELECT COUNT(*) AS count FROM {req.Table}";
    try
    {
        var rows = await QueryAsync(dbConnectionString, sql, Array.Empty<MySqlParameter>());
        var count = 0;
        if (rows.Count > 0 && rows[0].TryGetValue("count", out var value) && value is not null)
        {
            count = Convert.ToInt32(value);
        }
        return Results.Json(new { ok = true, worker = cfg.Name, count });
    }
    catch (Exception ex)
    {
        return Results.Json(new { ok = false, message = ex.Message }, statusCode: 500);
    }
});

app.Run();

static string GetConfigPath(string[] args)
{
    for (var i = 0; i < args.Length - 1; i++)
    {
        if (args[i] == "-config" || args[i] == "--config")
        {
            return args[i + 1];
        }
    }

    return Path.Combine("config", "worker-cs.json");
}

static WorkerConfig LoadConfig(string path)
{
    var json = File.ReadAllText(path);
    var options = new JsonSerializerOptions
    {
        PropertyNameCaseInsensitive = true,
    };
    return JsonSerializer.Deserialize<WorkerConfig>(json, options) ?? new WorkerConfig();
}

static string ResolveListenUrl(string address)
{
    if (address.StartsWith("http://", StringComparison.OrdinalIgnoreCase) ||
        address.StartsWith("https://", StringComparison.OrdinalIgnoreCase))
    {
        return address;
    }

    var port = address.Trim();
    if (port.StartsWith(":", StringComparison.OrdinalIgnoreCase))
    {
        port = port[1..];
    }

    if (!int.TryParse(port, out var portValue))
    {
        throw new InvalidOperationException("address must be a port number or URL");
    }

    return $"http://0.0.0.0:{portValue}";
}

static string BuildConnectionString(MySqlConfig cfg, bool includeDatabase)
{
    var builder = new MySqlConnectionStringBuilder
    {
        Server = cfg.Host,
        Port = (uint)cfg.Port,
        UserID = cfg.User,
        Password = cfg.Password,
        ConnectionTimeout = 5,
        DefaultCommandTimeout = 15,
        AllowUserVariables = true,
        Pooling = true,
    };

    if (includeDatabase)
    {
        builder.Database = cfg.Database;
    }

    return builder.ConnectionString;
}

static async Task EnsureDatabaseAsync(string serverConnectionString, string database)
{
    await using var conn = new MySqlConnection(serverConnectionString);
    await conn.OpenAsync();
    await using var cmd = conn.CreateCommand();
    cmd.CommandText = $"CREATE DATABASE IF NOT EXISTS `{database}`";
    await cmd.ExecuteNonQueryAsync();
}

static async Task DropAndRecreateDatabaseAsync(string serverConnectionString, string database)
{
    await using var conn = new MySqlConnection(serverConnectionString);
    await conn.OpenAsync();

    await using (var dropCmd = conn.CreateCommand())
    {
        dropCmd.CommandText = $"DROP DATABASE IF EXISTS `{database}`";
        await dropCmd.ExecuteNonQueryAsync();
    }

    await using (var createCmd = conn.CreateCommand())
    {
        createCmd.CommandText = $"CREATE DATABASE IF NOT EXISTS `{database}`";
        await createCmd.ExecuteNonQueryAsync();
    }
}

static async Task<int> ExecuteAsync(string connectionString, string sql, IEnumerable<MySqlParameter> parameters)
{
    await using var conn = new MySqlConnection(connectionString);
    await conn.OpenAsync();
    await using var cmd = conn.CreateCommand();
    cmd.CommandText = sql;
    foreach (var param in parameters)
    {
        cmd.Parameters.Add(param);
    }

    return await cmd.ExecuteNonQueryAsync();
}

static async Task<List<Dictionary<string, object?>>> QueryAsync(string connectionString, string sql, IEnumerable<MySqlParameter> parameters)
{
    await using var conn = new MySqlConnection(connectionString);
    await conn.OpenAsync();
    await using var cmd = conn.CreateCommand();
    cmd.CommandText = sql;
    foreach (var param in parameters)
    {
        cmd.Parameters.Add(param);
    }

    await using var reader = await cmd.ExecuteReaderAsync();
    var rows = new List<Dictionary<string, object?>>();
    while (await reader.ReadAsync())
    {
        var row = new Dictionary<string, object?>();
        for (var i = 0; i < reader.FieldCount; i++)
        {
            var value = reader.IsDBNull(i) ? null : reader.GetValue(i);
            row[reader.GetName(i)] = value;
        }
        rows.Add(row);
    }

    return rows;
}

static object? ConvertValue(JsonElement element)
{
    return element.ValueKind switch
    {
        JsonValueKind.String => element.GetString(),
        JsonValueKind.Number => element.TryGetInt64(out var value) ? value : element.GetDouble(),
        JsonValueKind.True => true,
        JsonValueKind.False => false,
        JsonValueKind.Null => DBNull.Value,
        _ => element.GetRawText(),
    };
}

static bool IsValidIdent(string? value)
{
    return !string.IsNullOrWhiteSpace(value) && ProgramRegex.IdentRegex.IsMatch(value);
}

public static class ProgramRegex
{
    public static readonly Regex IdentRegex = new("^[A-Za-z_][A-Za-z0-9_]*$", RegexOptions.Compiled);
    public static readonly Regex TypeRegex = new("^[A-Za-z0-9_(),\\s]+$", RegexOptions.Compiled);
}

sealed class WorkerConfig
{
    [JsonPropertyName("name")]
    public string? Name { get; set; }

    [JsonPropertyName("address")]
    public string? Address { get; set; }

    [JsonPropertyName("mysql")]
    public MySqlConfig MySql { get; set; } = new();
}

sealed class MySqlConfig
{
    [JsonPropertyName("host")]
    public string? Host { get; set; }

    [JsonPropertyName("port")]
    public int Port { get; set; }

    [JsonPropertyName("user")]
    public string? User { get; set; }

    [JsonPropertyName("password")]
    public string? Password { get; set; }

    [JsonPropertyName("database")]
    public string? Database { get; set; }
}

sealed class ColumnDef
{
    [JsonPropertyName("name")]
    public string? Name { get; set; }

    [JsonPropertyName("type")]
    public string? Type { get; set; }
}

sealed class CreateTableRequest
{
    [JsonPropertyName("table")]
    public string? Table { get; set; }

    [JsonPropertyName("columns")]
    public List<ColumnDef>? Columns { get; set; }

    [JsonPropertyName("primaryKey")]
    public string? PrimaryKey { get; set; }
}

sealed class DropTableRequest
{
    [JsonPropertyName("table")]
    public string? Table { get; set; }
}

sealed class InsertRequest
{
    [JsonPropertyName("table")]
    public string? Table { get; set; }

    [JsonPropertyName("values")]
    public Dictionary<string, JsonElement>? Values { get; set; }
}

sealed class SelectRequest
{
    [JsonPropertyName("table")]
    public string? Table { get; set; }

    [JsonPropertyName("where")]
    public string? Where { get; set; }

    [JsonPropertyName("limit")]
    public int? Limit { get; set; }

    [JsonPropertyName("dedupeKey")]
    public string? DedupeKey { get; set; }
}

sealed class UpdateRequest
{
    [JsonPropertyName("table")]
    public string? Table { get; set; }

    [JsonPropertyName("set")]
    public Dictionary<string, JsonElement>? Set { get; set; }

    [JsonPropertyName("where")]
    public string? Where { get; set; }
}

sealed class DeleteRequest
{
    [JsonPropertyName("table")]
    public string? Table { get; set; }

    [JsonPropertyName("where")]
    public string? Where { get; set; }
}

sealed class SummaryRequest
{
    [JsonPropertyName("table")]
    public string? Table { get; set; }
}
