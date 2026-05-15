import json
import os
import re

import pymysql
from flask import Flask, jsonify, request

config_path = os.getenv(
    "CONFIG_PATH",
    os.path.join(os.path.dirname(__file__), "..", "config", "worker-python.json"),
)

with open(config_path, "r", encoding="utf-8") as f:
    cfg = json.load(f)

# TODO: set mysql credentials in config/worker-python.json.
mysql_cfg = cfg.get("mysql", {})
host = mysql_cfg.get("host", "127.0.0.1")
port = int(mysql_cfg.get("port", 3306))
user = mysql_cfg.get("user")
password = mysql_cfg.get("password", "")
database = mysql_cfg.get("database")
connect_timeout = int(mysql_cfg.get("connectTimeout", 5))
read_timeout = int(mysql_cfg.get("readTimeout", 10))
write_timeout = int(mysql_cfg.get("writeTimeout", 10))

ident_re = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
type_re = re.compile(r"^[A-Za-z0-9_(),\s]+$")

if not user:
    raise ValueError("mysql.user is required")
if not database:
    raise ValueError("mysql.database is required")
if not ident_re.match(database):
    raise ValueError("mysql.database must be a simple identifier")

app = Flask(__name__)


def valid_ident(value: str) -> bool:
    return bool(ident_re.match(value or ""))


def valid_type(value: str) -> bool:
    return bool(type_re.match(value or ""))


def fail(status: int, msg: str):
    return jsonify(ok=False, message=msg), status


def ensure_db():
    conn = pymysql.connect(
        host=host,
        port=port,
        user=user,
        password=password,
        autocommit=True,
        connect_timeout=connect_timeout,
        read_timeout=read_timeout,
        write_timeout=write_timeout,
    )
    try:
        with conn.cursor() as cur:
            cur.execute(f"CREATE DATABASE IF NOT EXISTS `{database}`")
    finally:
        conn.close()


def get_conn():
    return pymysql.connect(
        host=host,
        port=port,
        user=user,
        password=password,
        database=database,
        autocommit=True,
        cursorclass=pymysql.cursors.DictCursor,
        connect_timeout=connect_timeout,
        read_timeout=read_timeout,
        write_timeout=write_timeout,
    )


ensure_db()


@app.get("/health")
def health():
    return jsonify(ok=True, message="ok")


@app.post("/table/create")
def create_table():
    data = request.get_json(silent=True) or {}
    table = data.get("table")
    columns = data.get("columns")
    primary_key = data.get("primaryKey")

    if not valid_ident(table):
        return fail(400, "invalid table name")
    if not isinstance(columns, list) or not columns:
        return fail(400, "columns required")

    defs = []
    for col in columns:
        name = col.get("name")
        col_type = str(col.get("type") or "").strip()
        if not valid_ident(name):
            return fail(400, "invalid column name")
        if not col_type or not valid_type(col_type):
            return fail(400, "invalid column type")
        defs.append(f"{name} {col_type}")

    if primary_key:
        if not valid_ident(primary_key):
            return fail(400, "invalid primary key")
        defs.append(f"PRIMARY KEY ({primary_key})")

    sql = f"CREATE TABLE IF NOT EXISTS {table} ({', '.join(defs)})"
    with get_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(sql)

    return jsonify(ok=True, message="table created")


@app.post("/table/drop")
def drop_table():
    data = request.get_json(silent=True) or {}
    table = data.get("table")

    if not valid_ident(table):
        return fail(400, "invalid table name")

    sql = f"DROP TABLE IF EXISTS {table}"
    with get_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(sql)

    return jsonify(ok=True, message="table dropped")


@app.post("/insert")
def insert_row():
    data = request.get_json(silent=True) or {}
    table = data.get("table")
    values = data.get("values")

    if not valid_ident(table):
        return fail(400, "invalid table name")
    if not isinstance(values, dict) or not values:
        return fail(400, "values required")

    cols = sorted(values.keys())
    for col in cols:
        if not valid_ident(col):
            return fail(400, "invalid column name")

    placeholders = ", ".join(["%s"] * len(cols))
    sql = f"INSERT INTO {table} ({', '.join(cols)}) VALUES ({placeholders})"

    with get_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(sql, [values[c] for c in cols])

    return jsonify(ok=True, message="inserted")


@app.post("/select")
def select_rows():
    data = request.get_json(silent=True) or {}
    table = data.get("table")
    where = data.get("where")
    limit = data.get("limit")

    if not valid_ident(table):
        return fail(400, "invalid table name")

    sql = f"SELECT * FROM {table}"
    if str(where or "").strip():
        sql += f" WHERE {where}"
    if isinstance(limit, int) and limit > 0:
        sql += f" LIMIT {limit}"

    with get_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(sql)
            rows = cur.fetchall()

    return jsonify(ok=True, rows=rows, count=len(rows))


@app.post("/update")
def update_rows():
    data = request.get_json(silent=True) or {}
    table = data.get("table")
    set_values = data.get("set")
    where = data.get("where")

    if not valid_ident(table):
        return fail(400, "invalid table name")
    if not isinstance(set_values, dict) or not set_values:
        return fail(400, "set values required")

    cols = sorted(set_values.keys())
    for col in cols:
        if not valid_ident(col):
            return fail(400, "invalid column name")

    set_clause = ", ".join([f"{c} = %s" for c in cols])
    sql = f"UPDATE {table} SET {set_clause}"
    if str(where or "").strip():
        sql += f" WHERE {where}"

    with get_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(sql, [set_values[c] for c in cols])
            count = cur.rowcount

    return jsonify(ok=True, message=f"updated {count} rows")


@app.post("/delete")
def delete_rows():
    data = request.get_json(silent=True) or {}
    table = data.get("table")
    where = data.get("where")

    if not valid_ident(table):
        return fail(400, "invalid table name")

    sql = f"DELETE FROM {table}"
    if str(where or "").strip():
        sql += f" WHERE {where}"

    with get_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(sql)
            count = cur.rowcount

    return jsonify(ok=True, message=f"deleted {count} rows")


@app.post("/db/drop")
def drop_db():
    try:
        conn = pymysql.connect(
            host=host,
            port=port,
            user=user,
            password=password,
            autocommit=True,
            connect_timeout=connect_timeout,
            read_timeout=read_timeout,
            write_timeout=write_timeout,
        )
        try:
            with conn.cursor() as cur:
                cur.execute(f"DROP DATABASE IF EXISTS `{database}`")
                cur.execute(f"CREATE DATABASE IF NOT EXISTS `{database}`")
        finally:
            conn.close()
        return jsonify(ok=True, message="db dropped")
    except Exception as exc:
        return fail(500, str(exc))


@app.post("/special/summary")
def special_summary():
    data = request.get_json(silent=True) or {}
    table = data.get("table")

    if not valid_ident(table):
        return fail(400, "invalid table name")

    sql = f"SELECT COUNT(*) AS count FROM {table}"
    with get_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(sql)
            row = cur.fetchone()
            count = row["count"] if row else 0

    return jsonify(ok=True, worker=cfg.get("name", "worker-python"), count=count)


if __name__ == "__main__":
    port = int(cfg.get("address", 8003))
    app.run(host="0.0.0.0", port=port, debug=False)
