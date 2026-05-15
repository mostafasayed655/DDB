package ddb.worker;

import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.DeserializationFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.sun.net.httpserver.Headers;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.ResultSetMetaData;
import java.sql.SQLException;
import java.sql.Statement;
import java.util.ArrayList;
import java.util.Collections;
import java.util.Comparator;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.regex.Pattern;

public final class WorkerServer {

    private static final ObjectMapper MAPPER = new ObjectMapper()
            .configure(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES, false);
    private static final Pattern IDENT_RE = Pattern.compile("^[A-Za-z_][A-Za-z0-9_]*$");
    private static final Pattern TYPE_RE = Pattern.compile("^[A-Za-z0-9_(),\\s]+$");

    public static void main(String[] args) throws Exception {
        String configPath = getConfigPath(args);
        WorkerConfig cfg = loadConfig(configPath);

        if (cfg.name == null || cfg.name.isBlank()) {
            cfg.name = "worker-java";
        }
        if (cfg.address == null || cfg.address.isBlank()) {
            cfg.address = ":8003";
        }
        if (cfg.mysql == null) {
            cfg.mysql = new MySqlConfig();
        }
        if (cfg.mysql.host == null || cfg.mysql.host.isBlank()) {
            cfg.mysql.host = "127.0.0.1";
        }
        if (cfg.mysql.port == 0) {
            cfg.mysql.port = 3306;
        }
        if (cfg.mysql.params == null || cfg.mysql.params.isBlank()) {
            cfg.mysql.params = "allowPublicKeyRetrieval=true&useSSL=false&serverTimezone=UTC";
        }

        if (cfg.mysql.user == null || cfg.mysql.user.isBlank()) {
            throw new IllegalStateException("mysql.user is required");
        }
        if (cfg.mysql.database == null || cfg.mysql.database.isBlank()) {
            throw new IllegalStateException("mysql.database is required");
        }
        if (!IDENT_RE.matcher(cfg.mysql.database).matches()) {
            throw new IllegalStateException("mysql.database must be a simple identifier");
        }

        ensureDatabase(cfg.mysql);

        int port = parsePort(cfg.address);
        HttpServer server = HttpServer.create(new InetSocketAddress("0.0.0.0", port), 0);
        server.createContext("/health", exchange -> {
            if (!"GET".equalsIgnoreCase(exchange.getRequestMethod())) {
                writeJson(exchange, 405, Map.of("ok", false, "message", "method not allowed"));
                return;
            }
            writeJson(exchange, 200, Map.of("ok", true, "message", "ok"));
        });
        server.createContext("/", exchange -> handleRequest(exchange, cfg));
        server.setExecutor(null);
        server.start();

        System.out.printf("worker %s listening on %d%n", cfg.name, port);
    }

    private static void handleRequest(HttpExchange exchange, WorkerConfig cfg) throws IOException {
        String path = exchange.getRequestURI().getPath();
        if ("/health".equals(path)) {
            return;
        }

        if (!"POST".equalsIgnoreCase(exchange.getRequestMethod())) {
            writeJson(exchange, 405, Map.of("ok", false, "message", "method not allowed"));
            return;
        }

        Map<String, Object> body = readJson(exchange.getRequestBody());
        if (body == null) {
            writeJson(exchange, 400, Map.of("ok", false, "message", "invalid json"));
            return;
        }

        try {
            switch (path) {
                case "/table/create" ->
                    handleCreateTable(exchange, cfg.mysql, body);
                case "/table/drop" ->
                    handleDropTable(exchange, cfg.mysql, body);
                case "/insert" ->
                    handleInsert(exchange, cfg.mysql, body);
                case "/select" ->
                    handleSelect(exchange, cfg.mysql, body);
                case "/update" ->
                    handleUpdate(exchange, cfg.mysql, body);
                case "/delete" ->
                    handleDelete(exchange, cfg.mysql, body);
                case "/db/drop" ->
                    handleDropDatabase(exchange, cfg.mysql);
                case "/special/summary" ->
                    handleSummary(exchange, cfg, body);
                default ->
                    writeJson(exchange, 404, Map.of("ok", false, "message", "not found"));
            }
        } catch (Exception ex) {
            writeJson(exchange, 500, Map.of("ok", false, "message", ex.getMessage()));
        }
    }

    private static void handleCreateTable(HttpExchange exchange, MySqlConfig cfg, Map<String, Object> body) throws SQLException, IOException {
        String table = getString(body.get("table"));
        if (!isValidIdent(table)) {
            writeJson(exchange, 400, Map.of("ok", false, "message", "invalid table name"));
            return;
        }

        Object columnsObj = body.get("columns");
        if (!(columnsObj instanceof List<?> columns) || columns.isEmpty()) {
            writeJson(exchange, 400, Map.of("ok", false, "message", "columns required"));
            return;
        }

        List<String> defs = new ArrayList<>();
        for (Object colObj : columns) {
            Map<String, Object> col = asStringObjectMap(colObj);
            if (col == null) {
                writeJson(exchange, 400, Map.of("ok", false, "message", "invalid column"));
                return;
            }
            String name = getString(col.get("name"));
            String type = getString(col.get("type"));
            if (!isValidIdent(name)) {
                writeJson(exchange, 400, Map.of("ok", false, "message", "invalid column name"));
                return;
            }
            if (type == null || type.isBlank() || !TYPE_RE.matcher(type).matches()) {
                writeJson(exchange, 400, Map.of("ok", false, "message", "invalid column type"));
                return;
            }
            defs.add(name + " " + type.trim());
        }

        String primaryKey = getString(body.get("primaryKey"));
        if (primaryKey != null && !primaryKey.isBlank()) {
            if (!isValidIdent(primaryKey)) {
                writeJson(exchange, 400, Map.of("ok", false, "message", "invalid primary key"));
                return;
            }
            defs.add("PRIMARY KEY (" + primaryKey + ")");
        }

        String sql = "CREATE TABLE IF NOT EXISTS " + table + " (" + String.join(", ", defs) + ")";
        execute(cfg, sql, List.of());
        writeJson(exchange, 200, Map.of("ok", true, "message", "table created"));
    }

    private static void handleDropTable(HttpExchange exchange, MySqlConfig cfg, Map<String, Object> body) throws SQLException, IOException {
        String table = getString(body.get("table"));
        if (!isValidIdent(table)) {
            writeJson(exchange, 400, Map.of("ok", false, "message", "invalid table name"));
            return;
        }

        String sql = "DROP TABLE IF EXISTS " + table;
        execute(cfg, sql, List.of());
        writeJson(exchange, 200, Map.of("ok", true, "message", "table dropped"));
    }

    private static void handleInsert(HttpExchange exchange, MySqlConfig cfg, Map<String, Object> body) throws SQLException, IOException {
        String table = getString(body.get("table"));
        if (!isValidIdent(table)) {
            writeJson(exchange, 400, Map.of("ok", false, "message", "invalid table name"));
            return;
        }

        Map<String, Object> values = asStringObjectMap(body.get("values"));
        if (values == null || values.isEmpty()) {
            writeJson(exchange, 400, Map.of("ok", false, "message", "values required"));
            return;
        }

        List<String> columns = new ArrayList<>(values.keySet());
        columns.sort(Comparator.naturalOrder());
        for (String col : columns) {
            if (!isValidIdent(col)) {
                writeJson(exchange, 400, Map.of("ok", false, "message", "invalid column name"));
                return;
            }
        }

        List<Object> params = new ArrayList<>();
        for (String col : columns) {
            params.add(values.get(col));
        }

        String placeholders = String.join(", ", Collections.nCopies(columns.size(), "?"));
        String sql = "INSERT INTO " + table + " (" + String.join(", ", columns) + ") VALUES (" + placeholders + ")";
        execute(cfg, sql, params);
        writeJson(exchange, 200, Map.of("ok", true, "message", "inserted"));
    }

    private static void handleSelect(HttpExchange exchange, MySqlConfig cfg, Map<String, Object> body) throws SQLException, IOException {
        String table = getString(body.get("table"));
        if (!isValidIdent(table)) {
            writeJson(exchange, 400, Map.of("ok", false, "message", "invalid table name"));
            return;
        }

        StringBuilder sql = new StringBuilder("SELECT * FROM ").append(table);
        String where = getString(body.get("where"));
        if (where != null && !where.isBlank()) {
            sql.append(" WHERE ").append(where);
        }
        Integer limit = getInt(body.get("limit"));
        if (limit != null && limit > 0) {
            sql.append(" LIMIT ").append(limit);
        }

        List<Map<String, Object>> rows = query(cfg, sql.toString(), List.of());
        Map<String, Object> payload = new HashMap<>();
        payload.put("ok", true);
        payload.put("rows", rows);
        payload.put("count", rows.size());
        writeJson(exchange, 200, payload);
    }

    private static void handleUpdate(HttpExchange exchange, MySqlConfig cfg, Map<String, Object> body) throws SQLException, IOException {
        String table = getString(body.get("table"));
        if (!isValidIdent(table)) {
            writeJson(exchange, 400, Map.of("ok", false, "message", "invalid table name"));
            return;
        }

        Map<String, Object> setValues = asStringObjectMap(body.get("set"));
        if (setValues == null || setValues.isEmpty()) {
            writeJson(exchange, 400, Map.of("ok", false, "message", "set values required"));
            return;
        }

        List<String> columns = new ArrayList<>(setValues.keySet());
        columns.sort(Comparator.naturalOrder());
        for (String col : columns) {
            if (!isValidIdent(col)) {
                writeJson(exchange, 400, Map.of("ok", false, "message", "invalid column name"));
                return;
            }
        }

        List<Object> params = new ArrayList<>();
        List<String> setClause = new ArrayList<>();
        for (String col : columns) {
            setClause.add(col + " = ?");
            params.add(setValues.get(col));
        }

        StringBuilder sql = new StringBuilder("UPDATE ").append(table).append(" SET ").append(String.join(", ", setClause));
        String where = getString(body.get("where"));
        if (where != null && !where.isBlank()) {
            sql.append(" WHERE ").append(where);
        }

        int affected = execute(cfg, sql.toString(), params);
        writeJson(exchange, 200, Map.of("ok", true, "message", "updated " + affected + " rows"));
    }

    private static void handleDelete(HttpExchange exchange, MySqlConfig cfg, Map<String, Object> body) throws SQLException, IOException {
        String table = getString(body.get("table"));
        if (!isValidIdent(table)) {
            writeJson(exchange, 400, Map.of("ok", false, "message", "invalid table name"));
            return;
        }

        StringBuilder sql = new StringBuilder("DELETE FROM ").append(table);
        String where = getString(body.get("where"));
        if (where != null && !where.isBlank()) {
            sql.append(" WHERE ").append(where);
        }

        int affected = execute(cfg, sql.toString(), List.of());
        writeJson(exchange, 200, Map.of("ok", true, "message", "deleted " + affected + " rows"));
    }

    private static void handleDropDatabase(HttpExchange exchange, MySqlConfig cfg) throws SQLException, IOException {
        dropAndRecreateDatabase(cfg);
        writeJson(exchange, 200, Map.of("ok", true, "message", "db dropped"));
    }

    private static void handleSummary(HttpExchange exchange, WorkerConfig cfg, Map<String, Object> body) throws SQLException, IOException {
        String table = getString(body.get("table"));
        if (!isValidIdent(table)) {
            writeJson(exchange, 400, Map.of("ok", false, "message", "invalid table name"));
            return;
        }

        String sql = "SELECT COUNT(*) AS count FROM " + table;
        List<Map<String, Object>> rows = query(cfg.mysql, sql, List.of());
        int count = 0;
        if (!rows.isEmpty()) {
            Object value = rows.get(0).get("count");
            if (value instanceof Number) {
                count = ((Number) value).intValue();
            } else if (value != null) {
                try {
                    count = Integer.parseInt(value.toString());
                } catch (NumberFormatException ignored) {
                    count = 0;
                }
            }
        }

        Map<String, Object> payload = new HashMap<>();
        payload.put("ok", true);
        payload.put("worker", cfg.name);
        payload.put("count", count);
        writeJson(exchange, 200, payload);
    }

    private static Map<String, Object> readJson(InputStream input) throws IOException {
        byte[] bytes = input.readAllBytes();
        String bodyText = new String(bytes, StandardCharsets.UTF_8).trim();
        if (bodyText.isEmpty()) {
            return new HashMap<>();
        }
        try {
            return MAPPER.readValue(bodyText, new TypeReference<Map<String, Object>>() {
            });
        } catch (Exception ex) {
            return null;
        }
    }

    private static void writeJson(HttpExchange exchange, int status, Map<String, Object> payload) throws IOException {
        byte[] data = MAPPER.writeValueAsBytes(payload);
        Headers headers = exchange.getResponseHeaders();
        headers.set("Content-Type", "application/json");
        exchange.sendResponseHeaders(status, data.length);
        try (OutputStream out = exchange.getResponseBody()) {
            out.write(data);
        }
    }

    private static void ensureDatabase(MySqlConfig cfg) throws SQLException {
        try (Connection conn = openConnection(cfg, false); Statement stmt = conn.createStatement()) {
            stmt.execute("CREATE DATABASE IF NOT EXISTS `" + cfg.database + "`");
        }
    }

    private static void dropAndRecreateDatabase(MySqlConfig cfg) throws SQLException {
        try (Connection conn = openConnection(cfg, false); Statement stmt = conn.createStatement()) {
            stmt.execute("DROP DATABASE IF EXISTS `" + cfg.database + "`");
            stmt.execute("CREATE DATABASE IF NOT EXISTS `" + cfg.database + "`");
        }
    }

    private static int execute(MySqlConfig cfg, String sql, List<Object> params) throws SQLException {
        try (Connection conn = openConnection(cfg, true); PreparedStatement stmt = conn.prepareStatement(sql)) {
            bindParams(stmt, params);
            return stmt.executeUpdate();
        }
    }

    private static List<Map<String, Object>> query(MySqlConfig cfg, String sql, List<Object> params) throws SQLException {
        try (Connection conn = openConnection(cfg, true); PreparedStatement stmt = conn.prepareStatement(sql)) {
            bindParams(stmt, params);
            try (ResultSet rs = stmt.executeQuery()) {
                List<Map<String, Object>> rows = new ArrayList<>();
                ResultSetMetaData meta = rs.getMetaData();
                int colCount = meta.getColumnCount();
                while (rs.next()) {
                    Map<String, Object> row = new LinkedHashMap<>();
                    for (int i = 1; i <= colCount; i++) {
                        row.put(meta.getColumnLabel(i), rs.getObject(i));
                    }
                    rows.add(row);
                }
                return rows;
            }
        }
    }

    private static void bindParams(PreparedStatement stmt, List<Object> params) throws SQLException {
        for (int i = 0; i < params.size(); i++) {
            stmt.setObject(i + 1, params.get(i));
        }
    }

    private static Connection openConnection(MySqlConfig cfg, boolean includeDatabase) throws SQLException {
        String url = buildJdbcUrl(cfg, includeDatabase);
        return DriverManager.getConnection(url, cfg.user, cfg.password == null ? "" : cfg.password);
    }

    private static String buildJdbcUrl(MySqlConfig cfg, boolean includeDatabase) {
        String host = cfg.host == null || cfg.host.isBlank() ? "127.0.0.1" : cfg.host;
        int port = cfg.port == 0 ? 3306 : cfg.port;
        String params = cfg.params == null || cfg.params.isBlank()
                ? "allowPublicKeyRetrieval=true&useSSL=false&serverTimezone=UTC"
                : cfg.params;
        String dbPart = includeDatabase ? "/" + cfg.database : "";
        return "jdbc:mysql://" + host + ":" + port + dbPart + "?" + params;
    }

    private static WorkerConfig loadConfig(String path) throws IOException {
        return MAPPER.readValue(new java.io.File(path), WorkerConfig.class);
    }

    private static String getConfigPath(String[] args) {
        for (int i = 0; i < args.length - 1; i++) {
            if (Objects.equals(args[i], "-config") || Objects.equals(args[i], "--config")) {
                return args[i + 1];
            }
        }
        return "config/worker-java.json";
    }

    private static int parsePort(String address) {
        if (address == null || address.isBlank()) {
            return 8003;
        }
        String trimmed = address.trim();
        try {
            if (trimmed.startsWith("http://") || trimmed.startsWith("https://")) {
                URI uri = URI.create(trimmed);
                return uri.getPort();
            }
            if (trimmed.startsWith(":")) {
                trimmed = trimmed.substring(1);
            } else if (trimmed.contains(":")) {
                trimmed = trimmed.substring(trimmed.lastIndexOf(':') + 1);
            }
            return Integer.parseInt(trimmed);
        } catch (Exception ex) {
            return 8003;
        }
    }

    private static boolean isValidIdent(String value) {
        return value != null && !value.isBlank() && IDENT_RE.matcher(value).matches();
    }

    private static String getString(Object value) {
        return value == null ? null : value.toString();
    }

    private static Integer getInt(Object value) {
        if (value instanceof Number num) {
            return num.intValue();
        }
        return null;
    }

    private static Map<String, Object> asStringObjectMap(Object value) {
        if (!(value instanceof Map<?, ?> raw)) {
            return null;
        }
        Map<String, Object> result = new LinkedHashMap<>();
        for (Map.Entry<?, ?> entry : raw.entrySet()) {
            if (entry.getKey() != null) {
                result.put(entry.getKey().toString(), entry.getValue());
            }
        }
        return result;
    }

    public static final class WorkerConfig {

        public String name;
        public String address;
        public MySqlConfig mysql;
    }

    public static final class MySqlConfig {

        public String host;
        public int port;
        public String user;
        public String password;
        public String database;
        public String params;
    }
}
