#!/usr/bin/env python3
"""Runs Lumen's real DDL and queries against an embedded ClickHouse engine (pip install chdb).

  LUMEN_DUMP_SQL=/tmp/sqldump.json go test ./internal/store -run Dump
  python3 scripts/test-sql.py /tmp/sqldump.json
"""
import json, sys, time, datetime
from chdb import session

dump = json.load(open(sys.argv[1]))
s = session.Session()
s.query("CREATE DATABASE lumen"); s.query("USE lumen")
for ddl in dump["ddl"]:
    s.query(ddl)
for ddl in dump["archive_ddl"]:
    s.query(ddl)

def ts(offset_s):
    t = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=offset_s)
    return t.strftime("%Y-%m-%d %H:%M:%S.") + "%09d" % (t.microsecond * 1000)

T1 = "5b8efff798038103d269b633813fc60c"
spans = [  # same shape the Go store sends (model.Span JSON)
    dict(tenant="acme", trace_id=T1, span_id="a1", parent_span_id="", name="GET /pay", service="checkout", kind=2, start_time=ts(-60), duration_ns=250_000_000, status_code=2, status_message="boom", attrs={"http.status_code": "500"}, resource_attrs={"service.name": "checkout"}),
    dict(tenant="acme", trace_id=T1, span_id="a2", parent_span_id="a1", name="SELECT", service="db", kind=3, start_time=ts(-59.9), duration_ns=100_000_000, status_code=0, status_message="", attrs={}, resource_attrs={}),
    dict(tenant="acme", trace_id="t2", span_id="b1", parent_span_id="", name="GET /home", service="web", kind=2, start_time=ts(-30), duration_ns=5_000_000, status_code=0, status_message="", attrs={}, resource_attrs={}),
    dict(tenant="other", trace_id="t3", span_id="c1", parent_span_id="", name="SECRET", service="web", kind=2, start_time=ts(-30), duration_ns=5_000_000, status_code=0, status_message="", attrs={}, resource_attrs={}),
]
logs = [
    dict(tenant="acme", ts=ts(-59.9), trace_id=T1, span_id="a1", severity="ERROR", service="checkout", body="Payment boom", attrs={"k": "v"}, resource_attrs={}),
    dict(tenant="acme", ts=ts(-20), trace_id="", span_id="", severity="INFO", service="web", body="hello", attrs={}, resource_attrs={}),
    dict(tenant="other", ts=ts(-20), trace_id="", span_id="", severity="INFO", service="web", body="other tenant", attrs={}, resource_attrs={}),
]
s.query("INSERT INTO otel_spans FORMAT JSONEachRow\n" + "\n".join(json.dumps(x) for x in spans))
s.query("INSERT INTO otel_logs FORMAT JSONEachRow\n" + "\n".join(json.dumps(x) for x in logs))

metrics = [dict(tenant="acme", ts=ts(-5), name="system.cpu.utilization", type="gauge", service="host", value=0.42, attrs={"core": "0"}),
    dict(tenant="acme", ts=ts(-1), name="cpu", type="gauge", service="host", value=0.2, attrs={"host": "h1"}),
    dict(tenant="acme", ts=ts(-1), name="cpu", type="gauge", service="host", value=0.6, attrs={"host": "h2"}),
    dict(tenant="other", ts=ts(-1), name="cpu", type="gauge", service="host", value=99, attrs={"host": "h9"})]
# a counter with a reset, one point per minute: 100,160,220,20(reset),80,140
for i, v in enumerate([100, 160, 220, 20, 80, 140]):
    metrics.append(dict(tenant="acme", ts=ts(-300 + 60 * i), name="reqs_total", type="sum", service="api", value=v, attrs={"host": "h1"}))
s.query("INSERT INTO otel_metrics FORMAT JSONEachRow\n" + "\n".join(json.dumps(x) for x in metrics))

def run(q):
    for k, v in q["Params"].items():
        s.query("SET param_%s = '%s'" % (k, v.replace("'", "\\'")))
    out = str(s.query(q["SQL"].replace("FORMAT JSONEachRow", "").strip() + " FORMAT JSONEachRow")).strip()
    return [json.loads(l) for l in out.splitlines() if l.strip()]

fail = 0
def check(name, cond, rows):
    global fail
    print(("PASS " if cond else "FAIL ") + name + ("" if cond else "  -> " + json.dumps(rows)[:300]))
    fail += 0 if cond else 1

Q = {q["Name"]: q for q in dump["queries"]}
try:
    r = run(Q["traces default"])
    check("traces default: 2 traces for tenant acme, newest first", len(r) == 2 and r[0]["trace_id"] == "t2", r)
    t = next(x for x in r if x["trace_id"] == T1)
    check("trace summary fields", t["root_name"] == "GET /pay" and t["root_service"] == "checkout" and int(t["span_count"]) == 2 and int(t["has_error"]) == 1 and int(t["duration_ns"]) >= 250_000_000 and "start_time" in t, t)
    r = run(Q["traces filtered"]); check("traces filtered (service+min duration+errors)", len(r) == 1 and r[0]["trace_id"] == T1, r)
    r = run(Q["get trace"]); check("get trace returns both spans", len(r) == 2 and {x["span_id"] for x in r} == {"a1", "a2"}, r)
    r = run(Q["logs default"]); check("logs default: tenant isolation", len(r) == 2 and all("other" not in x["body"] for x in r), r)
    r = run(Q["logs filtered"]); check("logs filtered (service+severity+text+trace)", len(r) == 1 and r[0]["body"] == "Payment boom", r)
    r = run(dict(SQL="SELECT count() AS n FROM otel_metrics WHERE name = 'system.cpu.utilization' FORMAT JSONEachRow", Params={}))
    check("metrics insert + read", int(r[0]["n"]) == 1, r)
    near = lambda a, b: abs(float(a) - b) < 1e-6
    r = run(Q["series gauge avg"]); check("gauge avg across hosts = 0.4, other tenant excluded", len(r) >= 1 and all(near(x["v"], 0.4) for x in r if x["g"] == "") , r)
    r = run(Q["series gauge sum by host"]); check("gauge sum grouped by host label", {x["g"]: round(float(x["v"]), 6) for x in r} == {"h1": 0.2, "h2": 0.6}, r)
    r = run(Q["series gauge filtered"]); check("label filter host=h2 (max)", len(r) == 1 and near(r[0]["v"], 0.6), r)
    r = run(Q["series gauge last"]); check("last value grouped by service", len(r) == 1 and r[0]["g"] == "host" and near(r[0]["v"], 0.8), r)
    r = run(Q["series rate"]); check("rate query returns one row per minute for the counter", len(r) == 6 and {x["sk"] for x in r} == {r[0]["sk"]} and sorted(float(x["iv"]) for x in r) == [20, 80, 100, 140, 160, 220], r)
    r = run(Q["series traces p95 by service"]); d = {x["g"]: float(x["v"]) for x in r}; check("trace p95 by service (entry spans only, ms)", near(d.get("checkout", -1), 250) and near(d.get("web", -1), 5) and "db" not in d, r)
    r = run(Q["series traces error_rate"]); d = {x["g"]: float(x["v"]) for x in r}; check("trace error rate %", near(d.get("checkout", -1), 100) and near(d.get("web", -1), 0), r)
    r = run(Q["series traces rps"]); check("trace rps", len(r) == 1 and near(r[0]["v"], 2 / 86400), r)
    r = run(Q["series logs by severity"]); d = {x["g"]: int(x["v"]) for x in r}; check("log counts by severity, tenant isolated", d == {"ERROR": 1, "INFO": 1}, r)
    r = run(Q["series logs filtered"]); check("log text + service filter", len(r) == 1 and int(r[0]["v"]) == 1, r)
    r = run(Q["services"]); d = {x["svc"]: sorted(x["signals"]) for x in r}; check("services with signals", d.get("checkout") == ["logs", "traces"] and d.get("host") == ["metrics"] and d.get("api") == ["metrics"] and "SECRET" not in str(r), r)
    r = run(Q["metric names"]); d = {x["name"]: x["mtype"] for x in r}; check("metric names with type", d.get("cpu") == "gauge" and d.get("reqs_total") == "sum", r)
    r = run(Q["metric labels"]); d = {x["label"]: sorted(x["vals"]) for x in r}; check("metric labels with values", d == {"host": ["h1", "h2"]}, r)

    # ---- retention: the TTL check reads create_table_query; ClickHouse prints INTERVAL 30 DAY as toIntervalDay(30)
    r = run(Q["ttl check"]); cq = r[0]["create_table_query"]
    check("ttl check sees the 30 day TTL as toIntervalDay(30)", "toIntervalDay(30)" in cq, r)
    s.query(dump["ttl_alter"]); r = run(Q["ttl check"])
    check("ALTER ... MODIFY TTL changes it (so a new LUMEN_RETENTION_DAYS applies to existing tables)", "toIntervalDay(14)" in r[0]["create_table_query"], r)
    ar = run(dict(SQL="SELECT create_table_query FROM system.tables WHERE database = 'lumen' AND name = 'otel_logs_archive' FORMAT JSONEachRow", Params={}))
    check("archive tables exist and have NO expiry", len(ar) == 1 and "TTL" not in ar[0]["create_table_query"], ar)

    # ---- backup: export one day for one tenant, load it into the archive, compare, unload
    today = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%d")
    for tb in ("otel_spans", "otel_logs", "otel_metrics"):
        r = run(Q["tenants " + tb]); check("tenants of " + tb + " today = acme and other", [x["tenant"] for x in r] == ["acme", "other"], r)
    exported = {}
    for tb in ("otel_spans", "otel_logs", "otel_metrics"):
        q = Q["export " + tb]
        for k, v in q["Params"].items():
            s.query("SET param_%s = '%s'" % (k, v))
        text = str(s.query(q["SQL"].replace("FORMAT JSONEachRow", "").strip() + " FORMAT JSONEachRow")).strip()
        exported[tb] = text
        rows = [json.loads(l) for l in text.splitlines() if l.strip()]
        check("export " + tb + " only has the tenant's rows", len(rows) > 0 and all(x["tenant"] == "acme" for x in rows), rows[:2])
    n_spans = len(exported["otel_spans"].splitlines())
    for tb in exported:  # exactly what the Go code posts: INSERT INTO <table>_archive FORMAT JSONEachRow + the exported lines
        s.query("INSERT INTO " + tb + "_archive FORMAT JSONEachRow\n" + exported[tb])
    for tb in exported:
        a = int(str(s.query("SELECT count() FROM " + tb + "_archive FORMAT CSV")).strip())
        b = len(exported[tb].splitlines())
        check("round trip " + tb + ": archive has the same row count (" + str(b) + ")", a == b and b > 0, [a, b])
    # values survive: maps, nanosecond timestamps, 64-bit ints, floats
    a = json.loads(str(s.query("SELECT * FROM otel_spans_archive WHERE span_id = 'a1' FORMAT JSONEachRow")).strip())
    o = json.loads(str(s.query("SELECT * FROM otel_spans WHERE span_id = 'a1' AND tenant = 'acme' FORMAT JSONEachRow")).strip())
    check("round trip keeps every column of a span exactly (map attrs, DateTime64(9), UInt64)", a == o, [a, o])
    a = json.loads(str(s.query("SELECT * FROM otel_metrics_archive WHERE name = 'system.cpu.utilization' FORMAT JSONEachRow")).strip())
    o = json.loads(str(s.query("SELECT * FROM otel_metrics WHERE name = 'system.cpu.utilization' AND tenant = 'acme' FORMAT JSONEachRow")).strip())
    check("round trip keeps a metric point exactly (Float64, labels)", a == o, [a, o])
    r = run(Q["loaded days"]); check("loaded days lists today for tenant acme", [x["d"] for x in r] == [today], r)
    # querying the archive: the Go code rewrites table names; do the same here and run the real series/log queries
    import re
    def arch(q): return dict(SQL=re.sub(r"\botel_(spans|logs|metrics)\b", r"otel_\1_archive", q["SQL"]), Params=q["Params"])
    r = run(arch(Q["logs default"])); check("the logs query runs against the archive table", len(r) == 2, r)
    r = run(arch(Q["series gauge sum by host"])); check("a metric series query runs against the archive table", {x["g"]: round(float(x["v"]), 6) for x in r} == {"h1": 0.2, "h2": 0.6}, r)
    # unload: only the tenant's rows of that day go, other tenants' archived rows stay
    s.query("INSERT INTO otel_logs_archive FORMAT JSONEachRow\n" + json.dumps(dict(tenant="other", ts=ts(-20), trace_id="", span_id="", severity="INFO", service="web", body="other tenant", attrs={}, resource_attrs={})))
    for tb in ("otel_spans", "otel_logs", "otel_metrics"):
        q = Q["unload " + tb]
        for k, v in q["Params"].items():
            s.query("SET param_%s = '%s'" % (k, v))
        s.query(q["SQL"] + " SETTINGS mutations_sync = 1")
    left = int(str(s.query("SELECT count() FROM otel_logs_archive FORMAT CSV")).strip())
    spans_left = int(str(s.query("SELECT count() FROM otel_spans_archive FORMAT CSV")).strip())
    check("unload removes the tenant's rows of the day and keeps other tenants' rows", left == 1 and spans_left == 0, [left, spans_left])
    # ---- removing a tenant: every row of ONE tenant goes from the live and the archive tables, nobody else's
    def cnt(tb, tenant): return int(str(s.query("SELECT count() FROM %s WHERE tenant = '%s' FORMAT CSV" % (tb, tenant))).strip())
    doomed_rows = {
        "otel_spans": dict(tenant="doomed", trace_id="td", span_id="d1", parent_span_id="", name="x", service="web", kind=2, start_time=ts(-30), duration_ns=1, status_code=0, status_message="", attrs={}, resource_attrs={}),
        "otel_logs": dict(tenant="doomed", ts=ts(-20), trace_id="", span_id="", severity="INFO", service="web", body="bye", attrs={}, resource_attrs={}),
        "otel_metrics": dict(tenant="doomed", ts=ts(-1), name="cpu", type="gauge", service="host", value=1, attrs={"host": "d1"}),
    }
    for tb, row in doomed_rows.items():
        for target in (tb, tb + "_archive"):
            s.query("INSERT INTO " + target + " FORMAT JSONEachRow\n" + json.dumps(row))
    before = {tb: (cnt(tb, "acme"), cnt(tb, "other")) for tb in doomed_rows}
    r = run(Q["tenant rows"]); check("the row count of a tenant sees live and archive tables", r and int(r[0]["n"]) == 6, r)
    for k, v in Q["purge otel_spans"]["Params"].items():
        s.query("SET param_%s = '%s'" % (k, v))
    for name in [k for k in Q if k.startswith("purge ")]:
        s.query(Q[name]["SQL"] + " SETTINGS mutations_sync = 1")
    check("purge: nothing is left of the tenant in any table", all(cnt(tb, "doomed") == 0 and cnt(tb + "_archive", "doomed") == 0 for tb in doomed_rows), [cnt(tb, "doomed") for tb in doomed_rows])
    r = run(Q["tenant rows"]); check("the row count agrees: 0", r and int(r[0]["n"]) == 0, r)
    after = {tb: (cnt(tb, "acme"), cnt(tb, "other")) for tb in doomed_rows}
    check("purge: other tenants' rows are untouched", before == after and all(a > 0 or o > 0 for a, o in after.values()), [before, after])
    # ---- up/down: newest value per series
    s.query("INSERT INTO otel_metrics FORMAT JSONEachRow\n" + "\n".join(json.dumps(x) for x in [
        dict(tenant="acme", ts=ts(-100), name="nextcloud_up", type="gauge", service="ks", value=1, attrs={"instance": "a.example.com", "host": "h1"}),
        dict(tenant="acme", ts=ts(-5), name="nextcloud_up", type="gauge", service="ks", value=0, attrs={"instance": "a.example.com", "host": "h1"}),
        dict(tenant="acme", ts=ts(-5), name="container_up", type="gauge", service="host", value=1, attrs={"container": "app", "host": "h1", "state": "running"}),
        dict(tenant="acme", ts=ts(-5), name="cpu_other", type="gauge", service="host", value=1, attrs={}),
        dict(tenant="other", ts=ts(-5), name="nextcloud_up", type="gauge", service="x", value=1, attrs={"instance": "z.example.com"})]))
    r = run(Q["latest"]); d = {(x["name"], x["attrs"].get("instance") or x["attrs"].get("container")): (float(x["v"]), int(x["t"])) for x in r}
    check("latest returns the NEWEST value per series (down now, although it was up 100 s ago), tenant isolated, only the named metrics",
          set(d) == {("nextcloud_up", "a.example.com"), ("container_up", "app")} and d[("nextcloud_up", "a.example.com")][0] == 0.0 and abs(d[("nextcloud_up", "a.example.com")][1] - time.time()) < 60, r)

    # ---- dropdown lists and filters (tenant "facets")
    fs = [
      dict(tenant="facets", trace_id="f1", span_id="s1", parent_span_id="", name="GET /pay", service="checkout", kind=2, start_time=ts(-60), duration_ns=1_000_000, status_code=0, status_message="", attrs={}, resource_attrs={"host.name": "web1"}),
      dict(tenant="facets", trace_id="f1", span_id="s2", parent_span_id="s1", name="SELECT", service="db", kind=3, start_time=ts(-59.9), duration_ns=1_000_000, status_code=0, status_message="", attrs={}, resource_attrs={"host.name": "db1"}),
      dict(tenant="facets", trace_id="f2", span_id="s3", parent_span_id="", name="GET /pay", service="checkout", kind=2, start_time=ts(-50), duration_ns=1_000_000, status_code=0, status_message="", attrs={}, resource_attrs={"host.name": "web1"}),
      dict(tenant="facets", trace_id="f3", span_id="s4", parent_span_id="", name="GET /home", service="web", kind=2, start_time=ts(-40), duration_ns=1_000_000, status_code=0, status_message="", attrs={}, resource_attrs={}),
    ]
    fl = [dict(tenant="facets", ts=ts(-30 - i), trace_id="", span_id="", severity="INFO", service=svc, body="x", attrs={}, resource_attrs=({"host.name": h} if h else {}))
          for i, (svc, h) in enumerate([("nginx", "web1"), ("nginx", "web1"), ("nginx", "web2"), ("system-logs", "web1"), ("sdk-app", "")])]
    s.query("INSERT INTO otel_spans FORMAT JSONEachRow\n" + "\n".join(json.dumps(x) for x in fs))
    s.query("INSERT INTO otel_logs FORMAT JSONEachRow\n" + "\n".join(json.dumps(x) for x in fl))
    def by(rows, k): return {x["v"]: int(x["n"]) for x in rows if x["k"] == k}
    r = run(Q["facets logs"])
    check("log facets: every service that sends logs, with counts", by(r, "service") == {"nginx": 3, "system-logs": 1, "sdk-app": 1}, r)
    check("log facets: every host, only rows that have one (the SDK without host.name does not add an empty entry)", by(r, "host") == {"web1": 3, "web2": 1}, r)
    check("log facets have no operations", by(r, "op") == {}, r)
    r = run(Q["facets traces"])
    check("trace facets: every service with spans", by(r, "service") == {"checkout": 2, "db": 1, "web": 1}, r)
    check("trace facets: operations are ROOT spans only (SELECT is not listed), most frequent first", by(r, "op") == {"GET /pay": 2, "GET /home": 1} and [x["v"] for x in r if x["k"] == "op"][0] == "GET /pay", r)
    check("trace facets: hosts", by(r, "host") == {"web1": 2, "db1": 1}, r)
    r = run(Q["facets traces service"])
    check("choosing a service narrows the operations list to that service", by(r, "op") == {"GET /pay": 2}, r)
    check("... but the services list still shows all of them", set(by(r, "service")) == {"checkout", "db", "web"}, r)
    r = run(Q["logs host filter"]); check("logs filtered by host web1 = 3 lines", len(r) == 3 and all(x["service"] in ("nginx", "system-logs") for x in r), r)
    r = run(Q["traces host filter"]); check("traces filtered by host web1 = the 2 traces whose spans ran there", sorted(x["trace_id"] for x in r) == ["f1", "f2"], r)
    r = run(Q["traces operation filter"]); check("traces filtered by root operation GET /pay = f1 and f2", sorted(x["trace_id"] for x in r) == ["f1", "f2"], r)
    r = run(Q["series logs host filter"]); check("the log volume chart can be limited to one host (3 lines on web1)", sum(int(x["v"]) for x in r) == 3, r)
    r = run(Q["series traces host filter"]); check("the trace chart can be limited to one host", sum(int(x["v"]) for x in r) == 2, r)
    ids = lambda r: sorted(x["trace_id"] for x in r)
    w, d_, b_ = ids(run(Q["traces group web1"])), ids(run(Q["traces group db1"])), ids(run(Q["traces group both"]))
    check("a host group of traces: web1 alone is f1 and f2", w == ["f1", "f2"], w)
    check("a group of two hosts is both hosts' traces together", b_ == sorted(set(w) | set(d_)) and len(d_) >= 1, [w, d_, b_])
    check("a group with a host that has no data adds nothing", ids(run(Q["traces group mixed"])) == w, run(Q["traces group mixed"]))
    check("a group with no hosts shows nothing", run(Q["traces group none"]) == [] and run(Q["logs group none"]) == [], [])
    lw, lb = run(Q["logs group web1"]), run(Q["logs group both"])
    check("a host group of logs: web1 alone is its 3 lines", len(lw) == 3, lw)
    check("and two hosts together are at least those", len(lb) >= 3 and {x["body"] for x in lw} <= {x["body"] for x in lb}, lb)
    check("a single host and a group together must both hold (web1 and db1 share no lines)", run(Q["logs host and group disagree"]) == [], run(Q["logs host and group disagree"]))
    r = run(Q["series logs group"]); check("the log volume chart limited to a group", sum(int(x["v"]) for x in r) >= 3, r)
    r = run(Q["series traces group"]); check("the trace chart limited to a group", sum(int(x["v"]) for x in r) >= 2, r)
    r = run(Q["series gauge hosts both"]); check("a metric limited to a group of hosts: h1+h2 summed", len(r) == 1 and near(r[0]["v"], 0.8), r)
    r = run(Q["series gauge hosts one and a stranger"]); check("another tenant's host in the group is never reached (only h1 = 0.2)", len(r) == 1 and near(r[0]["v"], 0.2), r)
    r = run(Q["series gauge hosts none"]); check("a metric limited to a group with no hosts is empty", r == [], r)
    r = run(Q["health disks"]); check("the disks of ClickHouse: name, free and total space", len(r) >= 1 and all(int(x["total"]) >= int(x["free"]) > 0 for x in r), r)
    r = run(Q["health tables"]); check("what takes the space: the tables, biggest first, the telemetry tables among them", len(r) >= 1 and any(x["name"].endswith("otel_spans") or x["name"].endswith("otel_logs") for x in r) and [int(x["bytes"]) for x in r] == sorted([int(x["bytes"]) for x in r], reverse=True), r)
    r = run(Q["traces service and operation filter"]); check("service + operation that do not match = nothing", r == [], r)
except Exception as e:
    print("FAIL exception:", str(e)[:600]); fail += 1
sys.exit(1 if fail else 0)
