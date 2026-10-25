#!/usr/bin/env python3
import argparse
import datetime as dt
import fcntl
import json
import os
import sqlite3
import sys
import tempfile
import urllib.error
import urllib.request


ALLOWED_UUIDS = {
    "6c8a8e8d-74a9-4467-8129-2bad82dae684",  # DMIT CORONA
    "0b46402c-62ab-4316-8b2d-22286b7f7bbf",  # DMIT TINY
    "e447ea90-ae90-4580-b8e8-d223b5bdb83d",  # NoSLA San Jose
}


def parse_time(value):
    value = str(value).strip().replace("Z", "+00:00")
    parsed = dt.datetime.fromisoformat(value)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.timezone.utc)
    return parsed.astimezone(dt.timezone.utc)


def iso_time(value):
    return value.astimezone(dt.timezone.utc).isoformat().replace("+00:00", "Z")


def cycle_key(now, reset_day):
    now = now.astimezone(dt.timezone.utc)
    reset_day = int(reset_day)
    if now.day >= reset_day:
        return f"{now.year:04d}-{now.month:02d}-{reset_day:02d}"
    previous_month = now.replace(day=1) - dt.timedelta(days=1)
    day = min(reset_day, previous_month.day)
    return f"{previous_month.year:04d}-{previous_month.month:02d}-{day:02d}"


def counter_delta(current, previous):
    current = max(0, int(current))
    previous = max(0, int(previous))
    return current - previous if current >= previous else current


def compute_used(billing_type, anchor_used, accumulated_up, accumulated_down):
    billing_type = str(billing_type).lower()
    if billing_type == "sum":
        increment = accumulated_up + accumulated_down
    elif billing_type == "max":
        increment = max(accumulated_up, accumulated_down)
    else:
        raise ValueError(f"unsupported billing_type: {billing_type}")
    return max(0, int(anchor_used) + int(increment))


def read_json(path):
    with open(path, "r", encoding="utf-8") as handle:
        return json.load(handle)


def atomic_write_json(path, value):
    directory = os.path.dirname(path)
    os.makedirs(directory, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".state-", suffix=".json", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as handle:
            json.dump(value, handle, ensure_ascii=False, indent=2)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def connect_read_only(path):
    return sqlite3.connect(f"file:{path}?mode=ro", uri=True, timeout=10)


def _counter_before(db, client_uuid, metric_name, target_ns=None):
    """Read old nanosecond or 1.6 normalized millisecond storage without writes."""
    columns = {row[1] for row in db.execute("pragma table_info(metric_rollups)")}
    if "series_id" in columns and "last_ts_milli" in columns:
        sql = ("select r.last_val, r.last_ts_milli from metric_rollups r "
               "join metric_series s on s.id = r.series_id "
               "join metric_resolutions d on d.id = r.resolution_id "
               "where s.metric_name = ? and s.entity_id = ?")
        params = [metric_name, client_uuid]
        if target_ns is not None:
            sql += " and r.last_ts_milli <= ?"
            params.append(target_ns // 1_000_000)
        sql += " order by r.last_ts_milli desc, d.resolution_milli asc limit 1"
        row = db.execute(sql, params).fetchone()
        if row:
            return int(row[0]), int(row[1]) * 1_000_000
    else:
        # During rollback/old installations the raw and rollup tables coexist.
        candidates = []
        tables = {r[0] for r in db.execute("select name from sqlite_master where type='table'")}
        for table, value_col, time_col in [("metric_points", "value", "ts_nano"),
                                           ("metric_rollups", "last_val", "last_ts")]:
            if table not in tables:
                continue
            sql = f"select {value_col}, {time_col} from {table} where metric_name = ? and entity_id = ?"
            params = [metric_name, client_uuid]
            if target_ns is not None:
                sql += f" and {time_col} <= ?"
                params.append(target_ns)
            sql += f" order by {time_col} desc limit 1"
            row = db.execute(sql, params).fetchone()
            if row:
                candidates.append((int(row[0]), int(row[1])))
        if candidates:
            return max(candidates, key=lambda value: value[1])
    raise RuntimeError(f"metric not found: {client_uuid} {metric_name}")


def get_latest_counter(db, client_uuid, metric_name):
    return _counter_before(db, client_uuid, metric_name)


def get_counter_at_or_before(db, client_uuid, metric_name, timestamp):
    target_ns = int(timestamp.timestamp()) * 1_000_000_000 + timestamp.microsecond * 1_000
    return _counter_before(db, client_uuid, metric_name, target_ns)


def load_collector_keys(main_db_path):
    with connect_read_only(main_db_path) as db:
        row = db.execute(
            "select value from configs where key = ?", ("official_traffic_sources",)
        ).fetchone()
    if not row:
        raise RuntimeError("official_traffic_sources is missing")
    sources = json.loads(row[0])
    if isinstance(sources, str):
        sources = json.loads(sources)
    result = {}
    for client_uuid in ALLOWED_UUIDS:
        source = sources.get(client_uuid, {})
        key = str(source.get("collector_key", "")).strip()
        if not key:
            raise RuntimeError(f"collector key missing: {client_uuid}")
        result[client_uuid] = key
    return result


def post_cache(endpoint, key, payload, timeout_seconds):
    request = urllib.request.Request(
        endpoint,
        data=json.dumps(payload, separators=(",", ":")).encode("utf-8"),
        method="POST",
        headers={
            "Authorization": f"Bearer {key}",
            "Content-Type": "application/json",
            "Accept": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout_seconds) as response:
            body = response.read().decode("utf-8", "replace")
            if response.status < 200 or response.status >= 300:
                raise RuntimeError(f"cache endpoint returned HTTP {response.status}")
            try:
                return json.loads(body)
            except json.JSONDecodeError as error:
                raise RuntimeError("cache endpoint returned invalid JSON") from error
    except urllib.error.HTTPError as error:
        body = error.read().decode("utf-8", "replace")[:300]
        raise RuntimeError(f"cache endpoint returned HTTP {error.code}: {body}") from error
    except urllib.error.URLError as error:
        raise RuntimeError(f"cache endpoint request failed: {error.reason}") from error


def validate_config(config):
    clients = config.get("clients", {})
    if set(clients) != ALLOWED_UUIDS:
        raise RuntimeError("refusing to run: config must contain exactly the approved DMIT/NoSLA UUIDs")
    for client_uuid, client in clients.items():
        if client.get("billing_type") not in {"sum", "max"}:
            raise RuntimeError(f"invalid billing type for {client_uuid}")
        if int(client.get("limit_bytes", 0)) <= 0:
            raise RuntimeError(f"invalid limit for {client_uuid}")
        if not 1 <= int(client.get("reset_day", 0)) <= 28:
            raise RuntimeError(f"invalid reset day for {client_uuid}")
        if int(client.get("anchor_used_bytes", -1)) < 0:
            raise RuntimeError(f"invalid official anchor for {client_uuid}")
        parse_time(client.get("anchor_updated_at"))


def load_reanchor_request(request_dir, client_uuid, limit_bytes):
    if not request_dir:
        return None
    path = os.path.join(request_dir, f"{client_uuid}.json")
    if not os.path.exists(path):
        return None
    request = read_json(path)
    if request.get("client_uuid") != client_uuid:
        raise RuntimeError(f"invalid reanchor request target: {client_uuid}")
    used_bytes = int(request.get("used_bytes", -1))
    if used_bytes < 0 or used_bytes > int(limit_bytes):
        raise RuntimeError(f"invalid reanchor value for {client_uuid}")
    return path, used_bytes


def reanchor_client_state(client, used_bytes, current_up, current_down, sample_ns, now):
    sample_time = dt.datetime.fromtimestamp(
        int(sample_ns) / 1_000_000_000, tz=dt.timezone.utc
    )
    return {
        "cycle_key": cycle_key(sample_time, client["reset_day"]),
        "anchor_updated_at": iso_time(sample_time),
        "anchor_official_used_bytes": int(used_bytes),
        "anchor_counter_up": int(current_up),
        "anchor_counter_down": int(current_down),
        "anchor_counter_sample_ns": int(sample_ns),
        "accumulated_up": 0,
        "accumulated_down": 0,
        "last_total_up": int(current_up),
        "last_total_down": int(current_down),
        "last_sample_ns": int(sample_ns),
        "initialized_at": iso_time(now),
    }


def initialize_client_state(metrics_db, client_uuid, client, now):
    anchor_time = parse_time(client["anchor_updated_at"])
    anchor_up, anchor_up_ts = get_counter_at_or_before(
        metrics_db, client_uuid, "net.total.up", anchor_time
    )
    anchor_down, anchor_down_ts = get_counter_at_or_before(
        metrics_db, client_uuid, "net.total.down", anchor_time
    )
    current_up, current_up_ts = get_latest_counter(metrics_db, client_uuid, "net.total.up")
    current_down, current_down_ts = get_latest_counter(metrics_db, client_uuid, "net.total.down")
    return {
        "cycle_key": cycle_key(anchor_time, client["reset_day"]),
        "anchor_updated_at": iso_time(anchor_time),
        "anchor_official_used_bytes": int(client["anchor_used_bytes"]),
        "anchor_counter_up": anchor_up,
        "anchor_counter_down": anchor_down,
        "anchor_counter_sample_ns": min(anchor_up_ts, anchor_down_ts),
        "accumulated_up": counter_delta(current_up, anchor_up),
        "accumulated_down": counter_delta(current_down, anchor_down),
        "last_total_up": current_up,
        "last_total_down": current_down,
        "last_sample_ns": min(current_up_ts, current_down_ts),
        "initialized_at": iso_time(now),
    }


def update_client_state(state, client, current_up, current_down, sample_ns, now):
    new_cycle = cycle_key(now, client["reset_day"])
    delta_up = counter_delta(current_up, state["last_total_up"])
    delta_down = counter_delta(current_down, state["last_total_down"])
    if state.get("cycle_key") != new_cycle:
        state["cycle_key"] = new_cycle
        state["anchor_updated_at"] = f"{new_cycle}T00:00:00Z"
        state["anchor_official_used_bytes"] = 0
        state["accumulated_up"] = delta_up
        state["accumulated_down"] = delta_down
    else:
        state["accumulated_up"] = int(state.get("accumulated_up", 0)) + delta_up
        state["accumulated_down"] = int(state.get("accumulated_down", 0)) + delta_down
    state["last_total_up"] = current_up
    state["last_total_down"] = current_down
    state["last_sample_ns"] = sample_ns
    return state


def run(config_path, dry_run=False):
    config = read_json(config_path)
    validate_config(config)
    now = dt.datetime.now(dt.timezone.utc)
    state_path = config["state_path"]
    state = read_json(state_path) if os.path.exists(state_path) else {"version": 1, "clients": {}}
    state.setdefault("clients", {})

    reanchor_requests = {}
    if not dry_run:
        for client_uuid, client in config["clients"].items():
            request = load_reanchor_request(
                config.get("reanchor_request_dir"),
                client_uuid,
                client["limit_bytes"],
            )
            if request:
                reanchor_requests[client_uuid] = request

    config_changed = False
    processed_request_paths = []

    with connect_read_only(config["metrics_db"]) as metrics_db:
        for client_uuid, client in config["clients"].items():
            if client_uuid in reanchor_requests:
                request_path, used_bytes = reanchor_requests[client_uuid]
                current_up, up_ts = get_latest_counter(
                    metrics_db, client_uuid, "net.total.up"
                )
                current_down, down_ts = get_latest_counter(
                    metrics_db, client_uuid, "net.total.down"
                )
                sample_ns = min(up_ts, down_ts)
                state["clients"][client_uuid] = reanchor_client_state(
                    client,
                    used_bytes,
                    current_up,
                    current_down,
                    sample_ns,
                    now,
                )
                sample_time = dt.datetime.fromtimestamp(
                    sample_ns / 1_000_000_000, tz=dt.timezone.utc
                )
                client["anchor_used_bytes"] = int(used_bytes)
                client["anchor_updated_at"] = iso_time(sample_time)
                config_changed = True
                processed_request_paths.append(request_path)
                print(
                    f"REANCHORED {client['name']} used={used_bytes} "
                    f"sample={iso_time(sample_time)}"
                )
                continue
            if client_uuid not in state["clients"]:
                state["clients"][client_uuid] = initialize_client_state(
                    metrics_db, client_uuid, client, now
                )
                continue
            current_up, up_ts = get_latest_counter(metrics_db, client_uuid, "net.total.up")
            current_down, down_ts = get_latest_counter(metrics_db, client_uuid, "net.total.down")
            sample_ns = min(up_ts, down_ts)
            update_client_state(
                state["clients"][client_uuid],
                client,
                current_up,
                current_down,
                sample_ns,
                now,
            )

    if config_changed:
        atomic_write_json(config_path, config)
    state["updated_at"] = iso_time(now)
    atomic_write_json(state_path, state)
    for request_path in processed_request_paths:
        try:
            os.unlink(request_path)
        except FileNotFoundError:
            pass
    keys = {} if dry_run else load_collector_keys(config["main_db"])

    for client_uuid, client in config["clients"].items():
        client_state = state["clients"][client_uuid]
        sample_time = dt.datetime.fromtimestamp(
            int(client_state["last_sample_ns"]) / 1_000_000_000,
            tz=dt.timezone.utc,
        )
        sample_age = max(0, (now - sample_time).total_seconds())
        if sample_age > int(config.get("max_sample_age_seconds", 600)):
            raise RuntimeError(
                f"latest WAN sample is stale for {client_uuid}: {int(sample_age)} seconds"
            )
        used = compute_used(
            client["billing_type"],
            client_state["anchor_official_used_bytes"],
            client_state["accumulated_up"],
            client_state["accumulated_down"],
        )
        limit_bytes = int(client["limit_bytes"])
        payload = {
            "client_uuid": client_uuid,
            "source_name": client["source_name"],
            "used_bytes": used,
            "limit_bytes": limit_bytes,
            "remaining_bytes": max(0, limit_bytes - used),
            "updated_at": iso_time(sample_time),
        }
        if dry_run:
            print(
                f"DRY-RUN {client['name']} type={client['billing_type']} "
                f"used={used} limit={limit_bytes} sample={payload['updated_at']}"
            )
            continue
        post_cache(
            config["endpoint"],
            keys[client_uuid],
            payload,
            int(config.get("request_timeout_seconds", 15)),
        )
        print(
            f"UPDATED {client['name']} type={client['billing_type']} "
            f"used={used} limit={limit_bytes} sample={payload['updated_at']}"
        )


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", required=True)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    lock_path = "/run/dmit-wan-calibrator.lock"
    with open(lock_path, "w", encoding="utf-8") as lock:
        try:
            fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            print("another calibrator run is active")
            return
        run(args.config, dry_run=args.dry_run)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(f"FAILED: {error}", file=sys.stderr)
        raise SystemExit(1)
