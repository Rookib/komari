import contextlib
import importlib.util
import io
import json
import pathlib
import sqlite3
import sys
import tempfile
import unittest
from unittest import mock
from zoneinfo import ZoneInfo


SPEC = importlib.util.spec_from_file_location('audit_data', pathlib.Path(__file__).with_name('audit_data.py'))
audit_data = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(audit_data)


class AuditDataTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.source = pathlib.Path(self.temp.name) / 'source'
        self.candidate = pathlib.Path(self.temp.name) / 'candidate'
        self.ns = 1792483200123456789
        for root in (self.source, self.candidate):
            root.mkdir()
            with sqlite3.connect(root / 'komari.db') as db:
                columns = audit_data.CLIENT_COLUMNS.split(',')
                db.execute('create table clients(' + ','.join(columns) + ')')
                row = ['private-node-uuid', 'private-token', 'private-name', 1, 30, 'USD', 1000,
                       'sum', 1, 'UTC', 0, 20, 30, 40, 50]
                db.execute('insert into clients values(' + ','.join('?' for _ in row) + ')', row)
                db.executescript('''
                    create table traffic_report_notifications(client text, enable bool, daily bool, weekly bool, monthly bool);
                    insert into traffic_report_notifications values('private-node-uuid', 1, 1, 0, 0);
                    create table ssh_login_notifications(client text, enable bool, ip_whitelist text);
                    insert into ssh_login_notifications values('private-node-uuid', 1, '["192.0.2.1"]');
                    create table traffic_reset_reminders(client text, reset_date text, sent_at text);
                    create table ssh_login_events(id text, client text, remote_ip text, occurred_at text, created_at text);
                    create table offline_notifications(client text, enable bool, grace_period integer, last_notified text);
                    insert into offline_notifications values('private-node-uuid', 1, 180, NULL);
                ''')
                stamp = ('2026-10-20 08:00:00.123456789' if root == self.source
                         else '2026-10-20T00:00:00.123456789Z')
                zero = '0001-01-01 00:00:00' if root == self.source else '0001-01-01T00:00:00Z'
                db.execute('insert into traffic_reset_reminders values(?,?,?)', ('private-node-uuid', '2026-10-20', stamp))
                db.execute('insert into ssh_login_events values(?,?,?,?,?)', ('private-event-id', 'private-node-uuid', '192.0.2.1', stamp, zero))
        with sqlite3.connect(self.source / 'metrics.db') as db:
            db.executescript('''
                create table metric_points(metric_name text, entity_id text, value real, ts_nano integer);
                create table metric_rollups(metric_name text, entity_id text, last_val real, last_ts integer);
            ''')
            for metric, value in [('net.total.up', 11), ('net.total.down', 22)]:
                db.execute('insert into metric_points values(?,?,?,?)', (metric, 'private-node-uuid', value, self.ns))
        with sqlite3.connect(self.candidate / 'metrics.db') as db:
            db.executescript('''
                create table metric_series(id integer, metric_name text, entity_id text);
                create table metric_resolutions(id integer, resolution_milli integer);
                create table metric_rollups(series_id integer, resolution_id integer, last_val real, last_ts_milli integer);
                insert into metric_resolutions values(1, 5000);
            ''')
            for index, (metric, value) in enumerate([('net.total.up', 11), ('net.total.down', 22)], 1):
                db.execute('insert into metric_series values(?,?,?)', (index, metric, 'private-node-uuid'))
                db.execute('insert into metric_rollups values(?,?,?,?)', (index, 1, value, self.ns // 1000000))

    def result(self):
        return audit_data.compare(audit_data.audit(self.source, 'Asia/Shanghai'),
                                  audit_data.audit(self.candidate, 'Asia/Shanghai'))

    def test_old_ns_new_ms_and_equivalent_notification_timestamps(self):
        result = self.result()
        self.assertTrue(result['passed'])
        self.assertEqual(result['traffic_counters'], 2)
        self.assertEqual(result['missing_traffic_counters']['source']['count'], 0)
        output = json.dumps(result)
        for private in ('private-node-uuid', 'private-event-id', 'private-token', '192.0.2.1'):
            self.assertNotIn(private, output)

    def test_lost_counter_fails(self):
        with sqlite3.connect(self.candidate / 'metrics.db') as db:
            db.execute('delete from metric_rollups where series_id=2')
        result = self.result()
        self.assertFalse(result['passed'])
        self.assertFalse(result['checks']['missing_traffic_counters_unchanged'])
        self.assertFalse(result['checks']['latest_traffic_counters_and_timestamps'])
        self.assertEqual(result['missing_traffic_counters']['candidate']['count'], 1)

    def test_both_empty_counter_sets_fail(self):
        for root, table in ((self.source, 'metric_points'), (self.candidate, 'metric_rollups')):
            with sqlite3.connect(root / 'metrics.db') as db:
                db.execute('delete from ' + table)
        result = self.result()
        self.assertFalse(result['passed'])
        self.assertFalse(result['checks']['source_traffic_counter_coverage'])
        self.assertTrue(result['checks']['missing_traffic_counters_unchanged'])

    def test_same_missing_offline_counter_does_not_require_all_nodes(self):
        with sqlite3.connect(self.source / 'metrics.db') as db:
            db.execute("delete from metric_points where metric_name='net.total.down'")
        with sqlite3.connect(self.candidate / 'metrics.db') as db:
            db.execute('delete from metric_rollups where series_id=2')
        self.assertTrue(self.result()['passed'])

    def test_rule_changes_fail_with_unchanged_row_counts(self):
        changes = (
            ('update traffic_report_notifications set weekly=1', 'update traffic_report_notifications set weekly=0'),
            ("update ssh_login_notifications set ip_whitelist='[]'", "update ssh_login_notifications set ip_whitelist='[\"192.0.2.1\"]'"),
            ("update traffic_reset_reminders set sent_at='2026-10-20T00:00:01.123456789Z'", "update traffic_reset_reminders set sent_at='2026-10-20T00:00:00.123456789Z'"),
        )
        for query, restore in changes:
            with self.subTest(query=query):
                with sqlite3.connect(self.candidate / 'komari.db') as db:
                    db.execute(query)
                result = self.result()
                self.assertTrue(result['checks']['node_and_custom_table_counts'])
                self.assertFalse(result['checks']['custom_notification_contents'])
                self.assertFalse(result['passed'])
                with sqlite3.connect(self.candidate / 'komari.db') as db:
                    db.execute(restore)

    def test_timestamp_epoch_precision_and_utc_default(self):
        normalize = lambda value: audit_data.normalize_timestamp(value, ZoneInfo('UTC'))
        self.assertEqual(normalize('1970-01-01T00:00:01.123456789Z'), {'utc_nanoseconds': 1123456789})
        self.assertEqual(normalize('2026-10-20 00:00:00'), normalize('2026-10-20T00:00:00Z'))
        self.assertEqual(normalize('1792483200123456789'), {'utc_nanoseconds': 1792483200123456789})
        self.assertIsNone(normalize(None))

    def test_invalid_timestamp_fails_without_leaking_value(self):
        with sqlite3.connect(self.candidate / 'komari.db') as db:
            db.execute("update traffic_reset_reminders set sent_at='private-invalid-value'")
        output = io.StringIO()
        argv = ['audit_data.py', '--source', str(self.source), '--candidate', str(self.candidate), '--legacy-timezone', 'Asia/Shanghai']
        with mock.patch.object(sys, 'argv', argv), contextlib.redirect_stdout(output):
            self.assertEqual(audit_data.main(), 1)
        self.assertEqual(json.loads(output.getvalue()), {'passed': False, 'error': 'ValueError'})


if __name__ == '__main__':
    unittest.main()
