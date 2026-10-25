#!/usr/bin/env python3
"""Compare frozen deployment snapshots without exposing stored credentials."""
import argparse
from contextlib import closing
import datetime as dt
import hashlib
import json
import pathlib
import re
import sqlite3
import sys
from zoneinfo import ZoneInfo

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / 'dmit-wan-calibrator'))
from dmit_wan_calibrator import get_latest_counter


COUNT_TABLES = (
    'clients', 'users', 'sessions', 'traffic_report_notifications',
    'traffic_reset_reminders', 'ssh_login_notifications', 'ssh_login_events',
    'ping_tasks', 'gpu_records_long_term', 'load_notifications',
)
# Hash every stored column in these retained tables, including whitelist/rule
# contents. Only these known time columns may change representation in migration.
NOTIFICATION_TIME_COLUMNS = {
    'traffic_report_notifications': (),
    'traffic_reset_reminders': ('sent_at',),
    'ssh_login_notifications': (),
    'ssh_login_events': ('occurred_at', 'created_at'),
    'offline_notifications': ('last_notified',),
    'load_notifications': ('last_notified',),
}
CLIENT_COLUMNS = (
    'uuid,token,name,price,billing_cycle,currency,traffic_limit,traffic_limit_type,'
    'traffic_reset_day,traffic_reset_timezone,traffic_reset_reported,'
    'vnstat_baseline_up,vnstat_baseline_down,vnstat_baseline_vn_up,vnstat_baseline_vn_down'
)
TIMESTAMP = re.compile(
    r'^(\d{4}-\d{2}-\d{2})(?:[T ](\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?'
    r'(Z|[+-]\d{2}:\d{2})?)?$'
)


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, ensure_ascii=False,
                                     default=str).encode()).hexdigest()


def normalize_timestamp(value, legacy_timezone):
    """Mirror the Go migration's timezone/epoch rules without losing nanoseconds."""
    if value is None:
        return None
    raw = str(value).strip()
    if not raw:
        return value
    if re.fullmatch(r'[+-]?\d+', raw):
        epoch = int(raw)
        magnitude = abs(epoch)
        multiplier = (1 if magnitude >= 10**17 else 1000 if magnitude >= 10**14
                      else 1000000 if magnitude >= 10**11 else 1000000000)
        return {'utc_nanoseconds': epoch * multiplier}
    match = TIMESTAMP.fullmatch(raw)
    if not match:
        # Never echo a database value into deployment logs on failure.
        raise ValueError('unsupported notification timestamp')
    day, clock, fraction, offset = match.groups()
    fraction_ns = int((fraction or '').ljust(9, '0'))
    stamp = dt.datetime.fromisoformat(day + 'T' + (clock or '00:00:00'))
    if stamp == dt.datetime.min and not fraction_ns and offset in (None, 'Z', '+00:00'):
        return {'zero_timestamp': True}
    if offset == 'Z':
        timezone = dt.timezone.utc
    elif offset:
        minutes = int(offset[1:3]) * 60 + int(offset[4:6])
        timezone = dt.timezone(dt.timedelta(minutes=minutes * (-1 if offset[0] == '-' else 1)))
    else:
        timezone = legacy_timezone
    delta = stamp.replace(tzinfo=timezone).astimezone(dt.timezone.utc) - dt.datetime(1970, 1, 1, tzinfo=dt.timezone.utc)
    return {'utc_nanoseconds': (delta.days * 86400 + delta.seconds) * 1000000000 + fraction_ns}


def notification_digest(db, table, time_columns, legacy_timezone):
    # Table/column identifiers originate only from the fixed allowlist/schema.
    columns = sorted(row[1] for row in db.execute('pragma table_info("' + table + '")'))
    quoted = ','.join('"' + column.replace('"', '""') + '"' for column in columns)
    rows = []
    for values in db.execute('select ' + quoted + ' from "' + table + '"'):
        row = dict(zip(columns, values))
        for column in time_columns:
            if column in row:
                row[column] = normalize_timestamp(row[column], legacy_timezone)
        rows.append(row)
    rows.sort(key=lambda row: json.dumps(row, sort_keys=True, default=str))
    return digest({'columns': columns, 'rows': rows})


def audit(root, legacy_timezone='UTC'):
    root = pathlib.Path(root).resolve()
    timezone = ZoneInfo(legacy_timezone)
    result = {'counts': {}, 'identity': {}, 'providers': {}, 'notifications': {},
              'counters': {}, 'missing_counters': []}
    with closing(sqlite3.connect((root / 'komari.db').as_uri() + '?mode=ro', uri=True)) as db:
        tables = {row[0] for row in db.execute("select name from sqlite_master where type='table'")}
        for table in COUNT_TABLES:
            if table in tables:
                result['counts'][table] = db.execute('select count(*) from ' + table).fetchone()[0]
        rows = db.execute('select ' + CLIENT_COLUMNS + ' from clients order by uuid').fetchall()
        result['identity']['clients'] = digest(rows)
        if 'users' in tables:
            result['identity']['users'] = digest(db.execute('select uuid,username,passwd,two_factor from users order by uuid').fetchall())
        if 'message_sender_providers' in tables:
            for name, addition in db.execute('select name,addition from message_sender_providers order by name'):
                result['providers'][name] = digest(addition)
        for table, time_columns in NOTIFICATION_TIME_COLUMNS.items():
            if table in tables:
                result['notifications'][table] = notification_digest(db, table, time_columns, timezone)
        uuids = [row[0] for row in rows]
    with closing(sqlite3.connect((root / 'metrics.db').as_uri() + '?mode=ro', uri=True)) as metrics:
        for uuid in uuids:
            for metric in ('net.total.up', 'net.total.down'):
                key = uuid + '/' + metric
                try:
                    value, ns = get_latest_counter(metrics, uuid, metric)
                    result['counters'][key] = [value, ns // 1000000]
                except RuntimeError:
                    result['missing_counters'].append(key)
    result['missing_counters'].sort()
    return result


def compare(before, after):
    checks = {
        'node_and_custom_table_counts': before['counts'] == after['counts'],
        'identity_billing_and_anchors': before['identity'] == after['identity'],
        'existing_notification_credentials': all(after['providers'].get(key) == value for key, value in before['providers'].items()),
        'custom_notification_contents': before['notifications'] == after['notifications'],
        'source_traffic_counter_coverage': bool(before['counters']),
        'missing_traffic_counters_unchanged': before['missing_counters'] == after['missing_counters'],
        'latest_traffic_counters_and_timestamps': before['counters'] == after['counters'],
    }
    missing = {
        name: {'count': len(data['missing_counters']), 'sha256': digest(data['missing_counters'])}
        for name, data in (('source', before), ('candidate', after))
    }
    return {'checks': checks, 'nodes': before['counts'].get('clients'),
            'traffic_counters': len(before['counters']), 'missing_traffic_counters': missing,
            'passed': all(checks.values())}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--source', required=True)
    parser.add_argument('--candidate', required=True)
    parser.add_argument('--legacy-timezone', default='UTC',
                        help='Must match the old service TZ, as used by the migration CLI.')
    args = parser.parse_args()
    try:
        result = compare(audit(args.source, args.legacy_timezone), audit(args.candidate, args.legacy_timezone))
    except (OSError, sqlite3.Error, ValueError, KeyError) as error:
        print(json.dumps({'passed': False, 'error': type(error).__name__}))
        return 1
    print(json.dumps(result))
    return 0 if result['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
