import datetime as dt
import importlib.util
import pathlib
import unittest
import sqlite3


MODULE_PATH = pathlib.Path(__file__).with_name("dmit_wan_calibrator.py")
SPEC = importlib.util.spec_from_file_location("calibrator", MODULE_PATH)
calibrator = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(calibrator)


class CalibratorTests(unittest.TestCase):
    def test_normalized_schema_counters_and_anchor(self):
        with sqlite3.connect(":memory:") as db:
            db.executescript("""
                create table metric_series(id integer, metric_name text, entity_id text);
                create table metric_resolutions(id integer, resolution_milli integer);
                create table metric_rollups(series_id integer, resolution_id integer, last_val real, last_ts_milli integer);
                insert into metric_series values(1, 'net.total.up', 'node'), (2, 'net.total.up', 'other');
                insert into metric_resolutions values(1, 1000), (2, 60000);
                insert into metric_rollups values(1, 1, 10, 1000), (1, 1, 20, 2000), (1, 2, 99, 2000), (2, 1, 100, 3000);
            """)
            self.assertEqual(calibrator.get_latest_counter(db, 'node', 'net.total.up'), (20, 2_000_000_000))
            boundary = dt.datetime.fromtimestamp(1.5, dt.timezone.utc)
            self.assertEqual(calibrator.get_counter_at_or_before(db, 'node', 'net.total.up', boundary), (10, 1_000_000_000))
            with self.assertRaises(RuntimeError):
                calibrator.get_latest_counter(db, 'missing', 'net.total.up')

    def test_legacy_schema_uses_newest_of_raw_and_rollup(self):
        with sqlite3.connect(":memory:") as db:
            db.executescript("""
                create table metric_points(metric_name text, entity_id text, value real, ts_nano integer);
                create table metric_rollups(metric_name text, entity_id text, last_val real, last_ts integer);
                insert into metric_points values('net.total.up', 'node', 10, 1000000000);
                insert into metric_rollups values('net.total.up', 'node', 20, 2000000000);
            """)
            self.assertEqual(calibrator.get_latest_counter(db, 'node', 'net.total.up'), (20, 2_000_000_000))
            boundary = dt.datetime.fromtimestamp(1.5, dt.timezone.utc)
            self.assertEqual(calibrator.get_counter_at_or_before(db, 'node', 'net.total.up', boundary), (10, 1_000_000_000))

    def test_counter_delta_survives_reboot(self):
        self.assertEqual(calibrator.counter_delta(150, 100), 50)
        self.assertEqual(calibrator.counter_delta(20, 100), 20)

    def test_sum_and_max(self):
        self.assertEqual(calibrator.compute_used("sum", 1000, 100, 200), 1300)
        self.assertEqual(calibrator.compute_used("max", 1000, 100, 200), 1200)

    def test_cycle_key(self):
        utc = dt.timezone.utc
        self.assertEqual(calibrator.cycle_key(dt.datetime(2026, 7, 21, tzinfo=utc), 22), "2026-06-22")
        self.assertEqual(calibrator.cycle_key(dt.datetime(2026, 7, 22, tzinfo=utc), 22), "2026-07-22")
        self.assertEqual(calibrator.cycle_key(dt.datetime(2026, 7, 8, tzinfo=utc), 9), "2026-06-09")
        self.assertEqual(calibrator.cycle_key(dt.datetime(2026, 7, 9, tzinfo=utc), 9), "2026-07-09")

    def test_reanchor_state_starts_from_exact_official_value(self):
        client = {"reset_day": 22}
        now = dt.datetime(2026, 7, 13, 15, 0, tzinfo=dt.timezone.utc)
        state = calibrator.reanchor_client_state(
            client, 1176477441720, 1000, 2000, int(now.timestamp() * 1e9), now
        )
        self.assertEqual(state["anchor_official_used_bytes"], 1176477441720)
        self.assertEqual(state["accumulated_up"], 0)
        self.assertEqual(state["accumulated_down"], 0)
        self.assertEqual(
            calibrator.compute_used(
                "sum",
                state["anchor_official_used_bytes"],
                state["accumulated_up"],
                state["accumulated_down"],
            ),
            1176477441720,
        )


if __name__ == "__main__":
    unittest.main()
