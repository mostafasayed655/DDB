const fs = require("fs");
const path = require("path");
const express = require("express");
const mysql = require("mysql2/promise");

// TODO: set mysql credentials in config/worker-node.json.
const configPath =
    process.env.CONFIG_PATH ||
    path.join(__dirname, "..", "config", "worker-node.json");
const cfg = JSON.parse(fs.readFileSync(configPath, "utf8"));

const mysqlCfg = cfg.mysql || {};
const host = mysqlCfg.host || "127.0.0.1";
const port = mysqlCfg.port || 3306;
const user = mysqlCfg.user;
const password = mysqlCfg.password || "";
const database = mysqlCfg.database;

const identRe = /^[A-Za-z_][A-Za-z0-9_]*$/;
const typeRe = /^[A-Za-z0-9_(),\s]+$/;

if (!user) {
    throw new Error("mysql.user is required");
}
if (!database) {
    throw new Error("mysql.database is required");
}
if (!identRe.test(database)) {
    throw new Error("mysql.database must be a simple identifier");
}

let pool;

function isValidIdent(value) {
    return identRe.test(value || "");
}

function isValidType(value) {
    return typeRe.test(value || "");
}

function fail(res, status, msg) {
    res.status(status).json({ ok: false, message: msg });
}

async function ensureDatabase() {
    const conn = await mysql.createConnection({
        host,
        port,
        user,
        password,
        multipleStatements: true,
    });
    await conn.query(`CREATE DATABASE IF NOT EXISTS \`${database}\``);
    await conn.end();
}

async function initPool() {
    await ensureDatabase();
    pool = mysql.createPool({
        host,
        port,
        user,
        password,
        database,
        waitForConnections: true,
        connectionLimit: 10,
        multipleStatements: true,
    });
}

async function run(sql, params = []) {
    const [result] = await pool.execute(sql, params);
    return result;
}

async function all(sql, params = []) {
    const [rows] = await pool.query(sql, params);
    return rows;
}

async function get(sql, params = []) {
    const rows = await all(sql, params);
    return rows[0] || null;
}

const app = express();
app.use(express.json());

app.get("/health", (req, res) => {
    res.json({ ok: true, message: "ok" });
});

app.post("/table/create", async (req, res) => {
    try {
        const { table, columns, primaryKey } = req.body || {};
        if (!isValidIdent(table)) {
            return fail(res, 400, "invalid table name");
        }
        if (!Array.isArray(columns) || columns.length === 0) {
            return fail(res, 400, "columns required");
        }

        const defs = [];
        for (const col of columns) {
            if (!isValidIdent(col.name)) {
                return fail(res, 400, "invalid column name");
            }
            const colType = String(col.type || "").trim();
            if (!colType || !isValidType(colType)) {
                return fail(res, 400, "invalid column type");
            }
            defs.push(`${col.name} ${colType}`);
        }
        if (primaryKey) {
            if (!isValidIdent(primaryKey)) {
                return fail(res, 400, "invalid primary key");
            }
            defs.push(`PRIMARY KEY (${primaryKey})`);
        }

        const sql = `CREATE TABLE IF NOT EXISTS ${table} (${defs.join(", ")})`;
        await run(sql);
        res.json({ ok: true, message: "table created" });
    } catch (err) {
        fail(res, 500, err.message);
    }
});

app.post("/table/drop", async (req, res) => {
    try {
        const { table } = req.body || {};
        if (!isValidIdent(table)) {
            return fail(res, 400, "invalid table name");
        }
        await run(`DROP TABLE IF EXISTS ${table}`);
        res.json({ ok: true, message: "table dropped" });
    } catch (err) {
        fail(res, 500, err.message);
    }
});

app.post("/insert", async (req, res) => {
    try {
        const { table, values } = req.body || {};
        if (!isValidIdent(table)) {
            return fail(res, 400, "invalid table name");
        }
        if (!values || typeof values !== "object") {
            return fail(res, 400, "values required");
        }

        const cols = Object.keys(values).sort();
        if (cols.length === 0) {
            return fail(res, 400, "values required");
        }
        for (const col of cols) {
            if (!isValidIdent(col)) {
                return fail(res, 400, "invalid column name");
            }
        }

        const placeholders = cols.map(() => "?").join(", ");
        const sql = `INSERT INTO ${table} (${cols.join(", ")}) VALUES (${placeholders})`;
        await run(sql, cols.map((c) => values[c]));
        res.json({ ok: true, message: "inserted" });
    } catch (err) {
        fail(res, 500, err.message);
    }
});

app.post("/select", async (req, res) => {
    try {
        const { table, where, limit } = req.body || {};
        if (!isValidIdent(table)) {
            return fail(res, 400, "invalid table name");
        }

        let sql = `SELECT * FROM ${table}`;
        if (String(where || "").trim() !== "") {
            sql += ` WHERE ${where}`;
        }
        if (Number(limit) > 0) {
            sql += ` LIMIT ${Number(limit)}`;
        }

        const rows = await all(sql);
        res.json({ ok: true, rows, count: rows.length });
    } catch (err) {
        fail(res, 500, err.message);
    }
});

app.post("/update", async (req, res) => {
    try {
        const { table, set, where } = req.body || {};
        if (!isValidIdent(table)) {
            return fail(res, 400, "invalid table name");
        }
        if (!set || typeof set !== "object") {
            return fail(res, 400, "set values required");
        }

        const cols = Object.keys(set).sort();
        if (cols.length === 0) {
            return fail(res, 400, "set values required");
        }
        for (const col of cols) {
            if (!isValidIdent(col)) {
                return fail(res, 400, "invalid column name");
            }
        }

        const setClause = cols.map((c) => `${c} = ?`).join(", ");
        let sql = `UPDATE ${table} SET ${setClause}`;
        if (String(where || "").trim() !== "") {
            sql += ` WHERE ${where}`;
        }

        const result = await run(sql, cols.map((c) => set[c]));
        res.json({ ok: true, message: `updated ${result.affectedRows || 0} rows` });
    } catch (err) {
        fail(res, 500, err.message);
    }
});

app.post("/delete", async (req, res) => {
    try {
        const { table, where } = req.body || {};
        if (!isValidIdent(table)) {
            return fail(res, 400, "invalid table name");
        }

        let sql = `DELETE FROM ${table}`;
        if (String(where || "").trim() !== "") {
            sql += ` WHERE ${where}`;
        }

        const result = await run(sql);
        res.json({ ok: true, message: `deleted ${result.affectedRows || 0} rows` });
    } catch (err) {
        fail(res, 500, err.message);
    }
});

app.post("/db/drop", async (req, res) => {
    try {
        if (pool) {
            await pool.end();
            pool = null;
        }
        const conn = await mysql.createConnection({
            host,
            port,
            user,
            password,
            multipleStatements: true,
        });
        await conn.query(`DROP DATABASE IF EXISTS \`${database}\``);
        await conn.query(`CREATE DATABASE IF NOT EXISTS \`${database}\``);
        await conn.end();
        await initPool();
        res.json({ ok: true, message: "db dropped" });
    } catch (err) {
        fail(res, 500, err.message);
    }
});

app.post("/special/summary", async (req, res) => {
    try {
        const { table } = req.body || {};
        if (!isValidIdent(table)) {
            return fail(res, 400, "invalid table name");
        }
        const row = await get(`SELECT COUNT(*) AS count FROM ${table}`);
        res.json({ ok: true, worker: cfg.name || "worker-node", count: row ? row.count : 0 });
    } catch (err) {
        fail(res, 500, err.message);
    }
});

const listenPort = typeof cfg.address === "number" ? cfg.address : parseInt(cfg.address, 10);
if (!listenPort) {
    throw new Error("address must be a port number");
}

async function start() {
    await initPool();
    app.listen(listenPort, () => {
        console.log(`worker ${cfg.name || "worker-node"} listening on ${listenPort}`);
    });
}

start().catch((err) => {
    console.error(err);
    process.exit(1);
});
