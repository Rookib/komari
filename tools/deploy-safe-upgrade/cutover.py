#!/usr/bin/env python3
"""Reviewable, host-local Komari cutover. Dry-run unless --execute is supplied.

Run --self-test locally; it uses temporary directories and fake commands only.
Production invocation requires root on the Docker host. It never pulls images,
deletes a backup/container, changes agents, or prints subprocess output/env.
After an interrupted run, use --rollback --run-dir <printed run directory>
--execute. Keep this script and its audit/calibrator dependencies on the host.
Rollback restores the pre-upgrade checkpoint. Writes received by the candidate
are retained in failed-data directories, not merged into the restored old DB.
"""
import argparse
import contextlib
import datetime
import fcntl
import hashlib
import json
import os
import pathlib
import re
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.request
import uuid

Path = pathlib.Path
ROOT = Path('/opt/komari/upgrade-1.6-safe-20261026')
DATA = Path('/opt/komari/data')
NGINX = Path('/etc/nginx/sites-available/000-komari-www-suckdry.net')
OLD_IMAGE = 'ghcr.io/suckdrygod/komari:1.2.6-tg.34-dmitnoslacal-local'
NEW_IMAGE = 'ghcr.io/rookib/komari:1.6.0-tg.35-safe'
VERSION = '1.6.0-tg.35-safe'
UNITS = ('dmit-wan-calibrator.timer', 'dmit-wan-calibrator.path',
         'dmit-wan-calibrator.service')
CALIBRATOR = Path('dmit-wan-calibrator/dmit_wan_calibrator.py')
# Exact, previously reviewed deletion. Do not generalize this into a config editor.
NGINX_REMOVAL = '''    # Preserve the deployed frontend while migrating its GitHub download URL.
    location = /assets/chunk-index-DFsjpN9i.js {
        alias /opt/komari/github-rename-20261026/chunk-patched.js;
        default_type application/javascript;
        add_header Cache-Control "no-cache" always;
    }

    location = /sw.js {
        proxy_pass http://127.0.0.1:25774;
        proxy_set_header Host $host;
        proxy_set_header Accept-Encoding "";
        proxy_hide_header Cache-Control;
        proxy_hide_header ETag;
        sub_filter_types application/javascript text/javascript;
        sub_filter_once on;
        sub_filter '{url:"assets/chunk-index-DFsjpN9i.js",revision:null}' '{url:"assets/chunk-index-DFsjpN9i.js",revision:"f1a3ac4e26aeedfb6ebfb960024dbce1bc3d672f2bed1b403e699d4b8816be19-rookib-2"}';
        add_header Cache-Control "no-cache" always;
    }

'''


class SafeFailure(Exception):
    """A fixed error code; never include command output or secret values."""


def require(condition, code):
    if not condition:
        raise SafeFailure(code)


def event(code, **safe_fields):
    print(json.dumps({'event': code, **safe_fields}), flush=True)


def private_write(path, value):
    """Atomic private journal/credential write with durable directory entry."""
    path = Path(path)
    temp = path.with_name(path.name + '.' + uuid.uuid4().hex + '.tmp')
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(value)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp, path)
        fsync_dir(path.parent)
    finally:
        if temp.exists():
            temp.unlink()


def fsync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def real_path(path, directory=False):
    path = Path(path)
    require(path.is_absolute() and path.resolve() == path, 'path_not_canonical')
    require(path.is_dir() if directory else path.is_file(), 'required_path_missing')
    require(not path.is_symlink(), 'symlink_path_rejected')
    return path


def tree_manifest(root):
    """Full bytes/metadata coverage, including WAL, keys and calibration state.

    Refuse links and device/socket files: an external target is not a complete
    independent backup. cp --reflink=never also avoids shared copy-on-write data.
    """
    root = real_path(root, directory=True)
    result = {}
    for path in [root, *sorted(root.rglob('*'))]:
        info = path.lstat()
        require(stat.S_ISDIR(info.st_mode) or stat.S_ISREG(info.st_mode),
                'data_contains_link_or_special_file')
        require(path.stat().st_dev == root.stat().st_dev, 'nested_mount_rejected')
        entry = {'mode': stat.S_IMODE(info.st_mode), 'uid': info.st_uid,
                 'gid': info.st_gid, 'mtime_ns': info.st_mtime_ns, 'kind': 'directory'}
        if hasattr(os, 'listxattr'):
            entry['xattrs'] = {name: hashlib.sha256(os.getxattr(path, name)).hexdigest()
                              for name in sorted(os.listxattr(path))}
        if stat.S_ISREG(info.st_mode):
            digest = hashlib.sha256()
            with path.open('rb') as stream:
                for chunk in iter(lambda: stream.read(1024 * 1024), b''):
                    digest.update(chunk)
            entry.update(kind='file', size=info.st_size, sha256=digest.hexdigest())
        result[str(path.relative_to(root))] = entry
    return result


def env_file(inspect):
    lines = inspect['Config'].get('Env') or []
    seen = set()
    for line in lines:
        require(isinstance(line, str) and '=' in line and
                not any(c in line for c in '\r\n\0'), 'unsupported_environment_value')
        key = line.split('=', 1)[0]
        require(re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', key) is not None and
                key not in seen, 'unsupported_environment_key')
        seen.add(key)
    return ('\n'.join(lines) + '\n').encode()


def validate_nginx(before, after):
    old_block = NGINX_REMOVAL.encode()
    require(before.count(old_block) == 1 and
            before.replace(old_block, b'', 1) == after,
            'nginx_change_is_not_exact_reviewed_removal')


class Runner:
    def __call__(self, args, timeout=120, check=True):
        # Never use a shell or echo arguments/output: docker inspect includes secrets.
        try:
            result = subprocess.run([str(x) for x in args], stdout=subprocess.PIPE,
                                    stderr=subprocess.PIPE, timeout=timeout,
                                    env={**os.environ, 'PYTHONDONTWRITEBYTECODE': '1'})
        except (OSError, subprocess.TimeoutExpired):
            raise SafeFailure('command_unavailable_or_timed_out') from None
        if check:
            require(result.returncode == 0, 'command_failed')
        return result


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, newurl):
        return None


class Cutover:
    def __init__(self, args, run=None, data=DATA, nginx=NGINX, runner=None):
        self.args, self.root = args, Path(args.root)
        self.run = Path(run) if run else self.root / (
            'cutover-' + datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
            + '-' + uuid.uuid4().hex[:8])
        self.data, self.nginx = Path(data), Path(nginx)
        self.cmd = runner or Runner()
        self.state = {'format': 1, 'phase': 'created', 'run': str(self.run),
                      'data': str(self.data), 'nginx': str(self.nginx)}
        self.calibrator_lock = None

    def save(self, **changes):
        self.state.update(changes)
        private_write(self.run / 'state.json', json.dumps(self.state, indent=2).encode())

    def mark(self, phase, **changes):
        self.save(phase=phase, **changes)
        event(phase)

    def recovery_record(self, **changes):
        """Journal/log failure must never prevent stopping writers or restoring data."""
        self.state.update(changes)
        try:
            self.save()
        except BaseException:
            try:
                event('recovery_journal_unavailable_status_kept_in_memory')
            except BaseException:
                pass

    @staticmethod
    def recovery_event(code, **safe_fields):
        try:
            event(code, **safe_fields)
        except BaseException:
            pass

    def inspect(self, container, missing_ok=False):
        result = self.cmd(['docker', 'container', 'inspect', container], check=False)
        if result.returncode:
            if missing_ok:
                # A daemon failure must not masquerade as a missing container.
                self.cmd(['docker', 'info', '--format', '{{.ServerVersion}}'])
                names = self.cmd(['docker', 'container', 'ls', '-a', '--format', '{{.Names}}'])
                require(container not in names.stdout.decode().splitlines(), 'container_inspect_failed')
                return None
            raise SafeFailure('container_inspect_failed')
        value = json.loads(result.stdout)
        require(isinstance(value, list) and len(value) == 1, 'container_inspect_shape')
        return value[0]

    def unit_states(self):
        result = {}
        for unit in UNITS:
            raw = self.cmd(['systemctl', 'show', unit, '--property=LoadState,ActiveState,UnitFileState']).stdout
            fields = dict(line.split('=', 1) for line in raw.decode().splitlines() if '=' in line)
            require(fields.get('LoadState') == 'loaded' and
                    fields.get('ActiveState') in ('active', 'inactive', 'failed'), 'calibrator_state_not_stable')
            result[unit] = fields
        return result

    def stop_units(self):
        # Stop triggers first; then wait for the writer service to become inactive.
        failed = False
        for unit in UNITS:
            try:
                self.cmd(['systemctl', 'stop', unit])
            except BaseException:
                failed = True
        states = self.unit_states()
        require(not failed and all(v['ActiveState'] in ('inactive', 'failed') for v in states.values()),
                'calibrator_not_stopped')

    def lock_calibrator(self):
        self.calibrator_lock = open('/run/dmit-wan-calibrator.lock', 'a')
        try:
            fcntl.flock(self.calibrator_lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            self.unlock_calibrator()
            raise SafeFailure('another_calibrator_writer_is_active') from None

    def unlock_calibrator(self):
        if self.calibrator_lock:
            self.calibrator_lock.close()
            self.calibrator_lock = None

    def restore_units(self):
        self.unlock_calibrator()
        current = self.unit_states()
        for unit, original in self.state['units'].items():
            require(current[unit]['UnitFileState'] == original['UnitFileState'],
                    'calibrator_enablement_changed_externally')
        # Start service before restoring its triggers, when it was originally active.
        # An already failed service is recorded, not deliberately failed again.
        for unit in reversed(UNITS):
            if self.state['units'][unit]['ActiveState'] == 'active':
                self.cmd(['systemctl', 'start', unit])
        restored = self.unit_states()
        for unit in UNITS[:2]:
            wanted = self.state['units'][unit]['ActiveState']
            require(restored[unit]['ActiveState'] == wanted or
                    wanted == 'failed' and restored[unit]['ActiveState'] in ('inactive', 'failed'),
                    'calibrator_trigger_state_not_restored')

    def stop_container(self, container):
        item = self.inspect(container)
        if item['State']['Running']:
            self.cmd(['docker', 'stop', '--time', '60', container], timeout=90)
        require(not self.inspect(container)['State']['Running'], 'container_not_stopped')

    def health(self, version=None, version_hash=None, timeout=90, expected_restarts=None):
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        deadline, good = time.monotonic() + timeout, 0
        restart_count = self.inspect('komari').get('RestartCount', 0)
        if expected_restarts is not None:
            require(restart_count == expected_restarts, 'container_restarted_before_health_check')
        while time.monotonic() < deadline:
            try:
                with opener.open('http://127.0.0.1:25774/api/version', timeout=3) as response:
                    require(response.status == 200, 'version_http_error')
                    value = json.loads(response.read(65537))
                require(value.get('status') == 'success', 'version_response_error')
                actual = value['data']
                require(isinstance(actual.get('version'), str), 'version_response_shape')
                if version is not None:
                    require(actual['version'] == version, 'wrong_service_version')
                if version_hash:
                    require(actual.get('hash') == version_hash, 'wrong_service_build')
                with opener.open('http://127.0.0.1:25774/', timeout=3) as response:
                    require(response.status == 200 and response.read(1), 'frontend_health_failed')
                item = self.inspect('komari')
                require(item['State']['Running'] and not item['State'].get('Restarting') and
                        item.get('RestartCount', 0) == restart_count, 'container_unstable')
                good += 1
                if good == 3:
                    return {'version': actual['version'], 'hash': actual.get('hash', '')}
            except Exception:
                good = 0
            time.sleep(1)
        raise SafeFailure('service_health_or_version_check_failed')

    def preflight(self):
        require(90 <= self.args.startup_timeout <= 3600, 'startup_timeout_must_be_90_to_3600_seconds')
        real_path(self.root, directory=True)
        require(self.root.stat().st_uid == 0 and self.root.stat().st_mode & 0o077 == 0,
                'upgrade_root_must_be_private_root_owned')
        real_path(self.data, directory=True)
        real_path(self.nginx)
        require(self.root.stat().st_dev == self.data.stat().st_dev, 'data_and_work_must_share_filesystem')
        require(self.root != self.data and self.data not in self.root.parents and
                self.root not in self.data.parents, 'data_and_work_paths_overlap')
        for artifact in (self.args.migrator, self.args.audit, self.args.calibrator, self.args.nginx_candidate):
            artifact = real_path(artifact)
            require(artifact.stat().st_uid == 0 and artifact.stat().st_mode & 0o022 == 0,
                    'artifact_must_be_root_owned_not_group_or_world_writable')
        validate_nginx(self.nginx.read_bytes(), Path(self.args.nginx_candidate).read_bytes())
        compile(Path(self.args.calibrator).read_bytes(), '<calibrator>', 'exec')
        require(Path(self.args.audit).parent.parent / 'dmit-wan-calibrator/dmit_wan_calibrator.py' ==
                Path(self.args.calibrator), 'audit_must_use_same_reviewed_calibrator')
        self.cmd(['nginx', '-t'])
        old = self.inspect('komari')
        cfg, host = old['Config'], old['HostConfig']
        require(old['State']['Running'] and cfg.get('Image') == OLD_IMAGE and
                cfg.get('Entrypoint') in (None, []) and cfg.get('Cmd') == ['/app/komari', 'server'],
                'unexpected_old_container')
        require(host.get('NetworkMode') == 'bridge' and
                host.get('RestartPolicy', {}).get('Name') == 'unless-stopped' and
                host.get('PortBindings') == {'25774/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '25774'}]},
                'unexpected_container_network_or_restart')
        mounts = old.get('Mounts') or []
        require(len(mounts) == 1 and mounts[0].get('Type') == 'bind' and
                mounts[0].get('Source') == str(self.data) and mounts[0].get('Destination') == '/app/data' and
                mounts[0].get('RW') is True, 'unexpected_container_mounts')
        require(not any(host.get(k) for k in ('Privileged', 'ReadonlyRootfs', 'CapAdd', 'CapDrop',
                    'Devices', 'DeviceRequests', 'ExtraHosts', 'Links', 'VolumesFrom',
                    'Memory', 'NanoCpus', 'CpuQuota', 'CpusetCpus', 'PidsLimit', 'SecurityOpt')) and
                cfg.get('User', '') in ('', 'root', '0') and cfg.get('WorkingDir', '') == '/app',
                'unhandled_container_runtime_settings')
        image = json.loads(self.cmd(['docker', 'image', 'inspect', self.args.image]).stdout)[0]
        require(image.get('Os') == 'linux' and image.get('Architecture') == 'amd64', 'wrong_candidate_architecture')
        old_version = self.health(timeout=12)
        env = env_file(old)
        timezone = next((line[3:] for line in cfg.get('Env', []) if line.startswith('TZ=')), 'UTC') or 'UTC'
        require(re.fullmatch(r'[A-Za-z0-9_+./-]+', timezone) is not None, 'invalid_original_timezone')
        self.run.mkdir(mode=0o700)
        private_write(self.run / 'container-inspect.private.json', json.dumps(old, indent=2).encode())
        private_write(self.run / 'container.env', env)
        private_write(self.run / 'nginx-before.conf', self.nginx.read_bytes())
        # Pin local artifacts before stopping service, preserving the audit import
        # layout. Later admin edits to the staging files cannot change this run.
        artifacts = self.run / 'artifacts'
        (artifacts / 'tools/deploy-safe-upgrade').mkdir(parents=True, mode=0o700)
        (artifacts / 'tools/dmit-wan-calibrator').mkdir(mode=0o700)
        pinned = {
            'migrator': artifacts / 'migrate-metrics',
            'audit': artifacts / 'tools/deploy-safe-upgrade/audit_data.py',
            'calibrator': artifacts / 'tools/dmit-wan-calibrator/dmit_wan_calibrator.py',
            'nginx_candidate': artifacts / 'nginx-after.conf',
        }
        artifact_hashes = {}
        for key, destination in pinned.items():
            content = Path(getattr(self.args, key)).read_bytes()
            private_write(destination, content)
            if key == 'migrator':
                destination.chmod(0o700)
            artifact_hashes[key] = hashlib.sha256(content).hexdigest()
            setattr(self.args, key, destination)
        self.save(old_id=old['Id'], old_image_id=old['Image'], old_name='komari',
                  preserved_name='komari-before-' + self.run.name, candidate_image_id=image['Id'],
                  units=self.unit_states(), timezone=timezone, old_version=old_version,
                  nginx_mode=stat.S_IMODE(self.nginx.stat().st_mode), nginx_uid=self.nginx.stat().st_uid,
                  nginx_gid=self.nginx.stat().st_gid, expected_version=self.args.version,
                  expected_hash=self.args.expected_hash,
                  startup_timeout=self.args.startup_timeout,
                  artifact_hashes=artifact_hashes,
                  migration_name='komari-migration-' + self.run.name)
        # Count full logical size (not sparse disk usage): copy + shadow migration
        # + independent restore must all fit without deleting any historical backup.
        size = sum(p.stat().st_size for p in self.data.rglob('*') if p.is_file())
        required = max(12 * 1024**3, 4 * size + 1024**3)
        require(shutil.disk_usage(self.root).free >= required, 'insufficient_space_for_backup_migration_and_rollback')
        event('preflight_passed', run_dir=str(self.run), required_free_bytes=required)

    def copy_tree(self, source, target):
        require(not target.exists(), 'copy_target_exists')
        self.cmd(['cp', '-a', '--reflink=never', '--', source, target], timeout=3600)
        self.cmd(['sync', '-f', target], timeout=120)

    def migrate_and_audit(self):
        candidate, backup = self.run / 'candidate-data', self.run / 'backup-data'
        self.mark('copying_independent_backup')
        before = tree_manifest(self.data)
        self.copy_tree(self.data, backup)
        require(tree_manifest(backup) == before and tree_manifest(self.data) == before,
                'backup_mismatch_or_source_changed')
        private_write(self.run / 'backup-manifest.json', json.dumps(before, sort_keys=True).encode())
        self.save(backup_ready=True)
        self.mark('copying_candidate')
        self.copy_tree(backup, candidate)
        self.mark('migrating_offline')
        self.cmd(['docker', 'create', '--name', self.state['migration_name'],
                  '--label', 'komari.safe-upgrade.run=' + self.run.name,
                  '--network', 'none', '--restart', 'no', '--read-only', '--cap-drop', 'ALL',
                  '--security-opt', 'no-new-privileges', '--tmpfs', '/tmp:rw,noexec,nosuid,size=256m',
                  '--mount', 'type=bind,src=' + str(candidate) + ',dst=/app/data',
                  '--mount', 'type=bind,src=' + str(backup) + ',dst=/backup,readonly',
                  '--mount', 'type=bind,src=' + str(self.args.migrator) + ',dst=/migrate,readonly',
                  '--entrypoint', '/migrate', self.state['candidate_image_id'],
                  '--data-dir', '/app/data', '--backup-dir', '/backup', '--legacy-timezone',
                  self.state['timezone'], '--confirm-offline'])
        self.cmd(['docker', 'start', '--attach', self.state['migration_name']], timeout=7200)
        migrated = self.inspect(self.state['migration_name'])
        require(not migrated['State']['Running'] and migrated['State'].get('ExitCode') == 0,
                'offline_migration_failed')
        self.mark('auditing_original_and_candidate')
        # Source is the stopped original, never the independent rollback backup.
        # SQLite read-only WAL access may update a transient SHM file on the source.
        result = self.cmd([sys.executable, self.args.audit, '--source', self.data, '--candidate', candidate,
                           '--legacy-timezone', self.state['timezone']], timeout=1800)
        report = json.loads(result.stdout)
        require(report.get('passed') is True and report.get('checks') and
                all(value is True for value in report['checks'].values()), 'audit_did_not_pass')
        # Current audit output contains only counts, booleans and digests.
        private_write(self.run / 'audit-result.json', json.dumps(report, sort_keys=True).encode())
        require(tree_manifest(backup) == before, 'backup_changed_during_migration')
        target = real_path(candidate / CALIBRATOR)
        info = target.stat()
        self.replace_file(target, Path(self.args.calibrator).read_bytes(),
                          stat.S_IMODE(info.st_mode), info.st_uid, info.st_gid)
        self.save(candidate_ready=True)

    @staticmethod
    def replace_file(path, content, mode, uid, gid):
        private_write(path, content)
        os.chown(path, uid, gid)
        os.chmod(path, mode)
        with open(path, 'rb') as stream:
            os.fsync(stream.fileno())
        fsync_dir(path.parent)

    def swap_data(self):
        require(self.state.get('candidate_ready'), 'candidate_not_verified')
        self.mark('switching_data', swap_intent=True)
        require(not (self.run / 'original-data').exists(), 'original_data_archive_exists')
        self.data.rename(self.run / 'original-data')
        fsync_dir(self.data.parent)
        fsync_dir(self.run)
        (self.run / 'candidate-data').rename(self.data)
        fsync_dir(self.data.parent)
        fsync_dir(self.run)
        self.save(data_switched=True)

    def start_candidate(self):
        self.mark('starting_candidate')
        self.cmd(['docker', 'create', '--name', 'komari', '--restart', 'unless-stopped',
                  '--network', 'bridge', '--publish', '127.0.0.1:25774:25774',
                  '--mount', 'type=bind,src=' + str(self.data) + ',dst=/app/data',
                  '--env-file', self.run / 'container.env', '--workdir', '/app',
                  '--label', 'komari.safe-upgrade.run=' + self.run.name,
                  '--entrypoint', '', self.state['candidate_image_id'], '/app/komari', 'server'])
        self.save(candidate_id=self.inspect('komari')['Id'])
        self.cmd(['docker', 'start', 'komari'])
        self.health(self.state['expected_version'], self.state['expected_hash'],
                    timeout=self.state['startup_timeout'], expected_restarts=0)

    def apply_nginx(self):
        # Recheck to avoid overwriting an unrelated admin edit during the migration.
        before = (self.run / 'nginx-before.conf').read_bytes()
        require(self.nginx.read_bytes() == before, 'nginx_changed_externally')
        candidate = Path(self.args.nginx_candidate).read_bytes()
        validate_nginx(before, candidate)
        self.mark('updating_nginx', nginx_intent=True)
        self.replace_file(self.nginx, candidate, self.state['nginx_mode'],
                          self.state['nginx_uid'], self.state['nginx_gid'])
        self.cmd(['nginx', '-t'])
        self.cmd(['systemctl', 'reload', 'nginx'])

    def execute(self):
        self.preflight()
        try:
            self.mark('stopping_writers', stop_intent=True)
            self.stop_units()
            self.lock_calibrator()
            self.stop_container(self.state['old_id'])
            self.mark('preserving_old_container', rename_intent=True)
            self.cmd(['docker', 'rename', self.state['old_id'], self.state['preserved_name']])
            self.migrate_and_audit()
            self.swap_data()
            self.start_candidate()
            self.apply_nginx()
            self.health(self.state['expected_version'], self.state['expected_hash'], expected_restarts=0)
            self.restore_units()
            self.mark('committed', committed=True)
            event('upgrade_complete_backups_and_old_container_retained', run_dir=str(self.run))
        except BaseException as error:
            self.recovery_record(failed_phase=self.state['phase'],
                                 failure_code=str(error) if isinstance(error, SafeFailure) else type(error).__name__)
            self.recovery_event('upgrade_failed_attempting_full_rollback', phase=self.state['phase'])
            try:
                self.rollback()
            except BaseException:
                self.recovery_record(rollback_failed=True)
                self.recovery_event('rollback_incomplete_keep_writers_stopped_manual_recovery_required', run_dir=str(self.run))
                raise SafeFailure('rollback_incomplete') from None
            raise SafeFailure('upgrade_failed_rolled_back') from None
        finally:
            self.unlock_calibrator()

    def stop_owned_new_containers(self):
        failed = False
        for name in (self.state.get('migration_name'), 'komari'):
            if not name:
                continue
            try:
                item = self.inspect(name, missing_ok=True)
                if not item or item['Id'] == self.state['old_id']:
                    continue
                require((item['Config'].get('Labels') or {}).get('komari.safe-upgrade.run') == self.run.name,
                        'unrelated_container_name_collision')
                self.stop_container(item['Id'])
                if name == 'komari':
                    self.cmd(['docker', 'rename', item['Id'], 'komari-failed-' + self.run.name + '-' + uuid.uuid4().hex[:6]])
            except BaseException:
                failed = True
        require(not failed, 'new_containers_not_safely_stopped')

    def restore_full_data(self):
        if not self.state.get('swap_intent'):
            return  # All migration writes were in the isolated candidate directory.
        require(self.state.get('backup_ready'), 'complete_backup_not_available')
        backup = self.run / 'backup-data'
        expected = json.loads((self.run / 'backup-manifest.json').read_bytes())
        require(tree_manifest(backup) == expected, 'rollback_backup_integrity_failed')
        # Never overlay or reuse a partly migrated directory. Preserve failed data.
        restored = None
        previous = self.state.get('restore_ready')
        if previous:
            previous = Path(previous)
            require(previous.parent == self.run and
                    re.fullmatch(r'restore-data-[0-9a-f]{8}', previous.name) is not None,
                    'invalid_recovery_directory_in_journal')
            # A completed copy left by an interrupted rollback is safe to reuse
            # only after verifying the whole manifest. Never reuse the live path.
            if previous.exists() and tree_manifest(previous) == expected:
                restored = previous
        if restored is None:
            restored = self.run / ('restore-data-' + uuid.uuid4().hex[:8])
            size = sum(v.get('size', 0) for v in expected.values())
            require(shutil.disk_usage(self.run).free >= size + 256 * 1024**2,
                    'insufficient_space_for_independent_restore')
            self.copy_tree(backup, restored)
            require(tree_manifest(restored) == expected, 'restored_data_integrity_failed')
        self.recovery_record(restore_ready=str(restored))
        if self.data.exists():
            self.data.rename(self.run / ('failed-data-' + uuid.uuid4().hex[:8]))
        restored.rename(self.data)
        fsync_dir(self.data.parent)
        fsync_dir(self.run)
        self.recovery_record(data_restored=True)

    def rollback(self):
        require(self.state.get('stop_intent'), 'run_has_not_changed_production')
        self.recovery_record(phase='rolling_back')
        self.recovery_event('rolling_back')
        # Try every writer stop even if a journal/systemctl/Docker step fails.
        # Never touch the data until all stop/identity/lock checks succeeded.
        stopped = True
        try:
            self.stop_units()
            if not self.calibrator_lock:
                self.lock_calibrator()
        except BaseException:
            stopped = False
        try:
            self.stop_owned_new_containers()
        except BaseException:
            stopped = False
        try:
            old = self.inspect(self.state['old_id'])
            require(old['Image'] == self.state['old_image_id'], 'old_container_identity_changed')
            self.stop_container(self.state['old_id'])
        except BaseException:
            stopped = False
        require(stopped, 'rollback_could_not_stop_all_writers')
        self.restore_full_data()
        if self.state.get('nginx_intent'):
            before = (self.run / 'nginx-before.conf').read_bytes()
            candidate = (self.run / 'artifacts/nginx-after.conf').read_bytes()
            require(self.nginx.read_bytes() in (before, candidate), 'nginx_changed_externally_during_rollback')
            self.replace_file(self.nginx, before,
                              self.state['nginx_mode'], self.state['nginx_uid'], self.state['nginx_gid'])
            self.cmd(['nginx', '-t'])
            self.cmd(['systemctl', 'reload', 'nginx'])
        if old['Name'] != '/komari':
            self.cmd(['docker', 'rename', self.state['old_id'], 'komari'])
        self.cmd(['docker', 'start', self.state['old_id']])
        self.health(self.state['old_version']['version'], self.state['old_version'].get('hash'))
        self.restore_units()
        self.recovery_record(phase='rolled_back', rolled_back=True)
        self.recovery_event('full_rollback_complete_backups_retained', run_dir=str(self.run))


def parser():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--execute', action='store_true', help='explicitly perform the reviewed host-local operation')
    p.add_argument('--rollback', action='store_true', help='restore an existing run, including the entire original data')
    p.add_argument('--run-dir', type=Path)
    p.add_argument('--self-test', action='store_true')
    p.add_argument('--root', type=Path, default=ROOT)
    p.add_argument('--image', default=NEW_IMAGE)
    p.add_argument('--version', default=VERSION)
    p.add_argument('--expected-hash', default='', help='recommended: require the reviewed /api/version build hash')
    p.add_argument('--startup-timeout', type=int, default=600,
                   help='first-start health deadline, 90..3600 seconds; choose from rehearsal timing')
    p.add_argument('--migrator', type=Path, default=ROOT / 'migrate-metrics-linux-amd64')
    p.add_argument('--audit', type=Path, default=ROOT / 'tools/deploy-safe-upgrade/audit_data.py')
    p.add_argument('--calibrator', type=Path, default=ROOT / 'tools/dmit-wan-calibrator/dmit_wan_calibrator.py')
    p.add_argument('--nginx-candidate', type=Path, default=ROOT / 'nginx-after.conf')
    return p


def main(argv=None):
    args = parser().parse_args(argv)
    if args.self_test:
        suite = unittest.defaultTestLoader.loadTestsFromTestCase(RollbackTests)
        return 0 if unittest.TextTestRunner(verbosity=2).run(suite).wasSuccessful() else 1
    require(not args.rollback or args.run_dir, 'rollback_requires_run_directory')
    require(not args.run_dir or args.rollback, 'run_directory_only_for_rollback')
    if not args.execute:
        event('dry_run_no_commands_or_files_changed', action='rollback' if args.rollback else 'upgrade',
              image=args.image, version=args.version, data=str(DATA), root=str(args.root),
              steps=['validate local artifacts and exact container configuration',
                     'save private inspect/env and original unit states',
                     'stop all writers; make full independent backup',
                     'migrate independent candidate offline; audit against stopped original',
                     'retain old container/data; switch candidate; check version and health',
                     'remove only two reviewed nginx overrides; restore trigger states',
                     'on failure: stop candidate, restore whole backup, old container, nginx and triggers'])
        return 0
    require(sys.platform.startswith('linux') and os.geteuid() == 0, 'execute_requires_root_linux_host')
    os.umask(0o077)
    real_path(args.root, directory=True)
    require(args.root.stat().st_uid == 0 and args.root.stat().st_mode & 0o077 == 0,
            'upgrade_root_must_be_private_root_owned')
    # Prevent concurrent cutover/rollback. Keep this harmless lock file for reuse.
    with open(args.root / '.cutover.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        operation = Cutover(args, args.run_dir)
        if args.rollback:
            run = real_path(args.run_dir, directory=True)
            require(run.parent == args.root and run.stat().st_uid == 0 and run.stat().st_mode & 0o077 == 0,
                    'untrusted_run_directory')
            journal = real_path(run / 'state.json')
            require(journal.stat().st_uid == 0 and journal.stat().st_mode & 0o077 == 0, 'untrusted_journal')
            operation.state = json.loads(journal.read_bytes())
            require(operation.state.get('format') == 1 and operation.state.get('run') == str(run) and
                    operation.state.get('data') == str(DATA) and operation.state.get('nginx') == str(NGINX),
                    'journal_paths_do_not_match_deployment')
            require(not operation.state.get('rolled_back'), 'run_already_rolled_back')
            try:
                operation.rollback()
            finally:
                operation.unlock_calibrator()
        else:
            operation.execute()
    return 0


class RollbackTests(unittest.TestCase):
    """No Docker, systemctl, network or production paths are used by these tests."""
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name).resolve()
        self.run = self.base / 'run'
        self.run.mkdir()
        self.data = self.base / 'data'
        self.data.mkdir()
        self.nginx = self.base / 'nginx.conf'
        self.nginx.write_text('old nginx')
        self.commands = []
        self.op = Cutover(parser().parse_args([]), self.run, self.data, self.nginx, self.fake_command)
        self.op.state.update(stop_intent=True, old_id='old-id', old_image_id='old-image', preserved_name='old',
                             migration_name='migration', units={u: {'ActiveState': 'inactive',
                             'UnitFileState': 'enabled'} for u in UNITS},
                             old_version={'version': 'old'}, nginx_mode=0o644,
                             nginx_uid=os.getuid(), nginx_gid=os.getgid())
        self.op.stop_units = lambda: self.commands.append(('stop-units',))
        self.op.lock_calibrator = lambda: None
        self.op.stop_owned_new_containers = lambda: self.commands.append(('stop-new',))
        self.op.stop_container = lambda name: self.commands.append(('stop', name))
        self.op.restore_units = lambda: self.commands.append(('restore-units',))
        self.op.health = lambda *args, **kwargs: self.commands.append(('health',))
        self.op.inspect = lambda *args, **kwargs: {'Image': 'old-image', 'Name': '/old', 'State': {'Running': False}}
        self.op.copy_tree = lambda source, target: shutil.copytree(source, target, copy_function=shutil.copy2)
        self.addCleanup(self.op.unlock_calibrator)

    def fake_command(self, command, **kwargs):
        self.commands.append(tuple(str(x) for x in command))
        return subprocess.CompletedProcess(command, 0, b'', b'')

    def make_backup(self):
        for name, body in {'komari.db': b'old-main', 'metrics.db': b'old-metrics',
                           'metrics.db-wal': b'old-wal', 'secret.key': b'private-key',
                           'dmit-wan-calibrator/state.json': b'old-state',
                           str(CALIBRATOR): b'old-script'}.items():
            target = self.data / name
            target.parent.mkdir(exist_ok=True)
            target.write_bytes(body)
            target.chmod(0o600)
        backup = self.run / 'backup-data'
        shutil.copytree(self.data, backup, copy_function=shutil.copy2)
        private_write(self.run / 'backup-manifest.json', json.dumps(tree_manifest(backup)).encode())
        self.op.state.update(backup_ready=True, swap_intent=True)
        return tree_manifest(backup)

    def test_partial_backup_failure_leaves_original_untouched(self):
        (self.data / 'komari.db').write_bytes(b'original')
        (self.run / 'backup-data').mkdir()
        self.op.rollback()
        self.assertEqual((self.data / 'komari.db').read_bytes(), b'original')
        self.assertIn(('docker', 'start', 'old-id'), self.commands)

    def test_full_rollback_restores_wal_keys_and_state_and_retains_failed_tree(self):
        expected = self.make_backup()
        (self.data / 'komari.db').write_bytes(b'migrated')
        (self.data / 'new-only.db').write_bytes(b'new-format')
        (self.data / CALIBRATOR).write_bytes(b'new-script')
        (self.run / 'nginx-before.conf').write_bytes(b'old nginx')
        (self.run / 'artifacts').mkdir()
        (self.run / 'artifacts/nginx-after.conf').write_bytes(b'new nginx')
        self.nginx.write_bytes(b'new nginx')
        self.op.state['nginx_intent'] = True
        self.op.rollback()
        self.assertEqual(tree_manifest(self.data), expected)
        self.assertEqual(self.nginx.read_bytes(), b'old nginx')
        self.assertEqual(len(list(self.run.glob('failed-data-*'))), 1)
        self.assertTrue((self.run / 'backup-data').exists())
        self.assertLess(self.commands.index(('stop-new',)), self.commands.index(('docker', 'start', 'old-id')))
        self.assertLess(self.commands.index(('nginx', '-t')), self.commands.index(('docker', 'start', 'old-id')))
        self.assertEqual(self.commands[-1], ('restore-units',))

    def test_interrupted_swap_with_missing_live_directory_is_recovered(self):
        expected = self.make_backup()
        self.data.rename(self.run / 'original-data')
        self.op.rollback()
        self.assertEqual(tree_manifest(self.data), expected)
        self.assertTrue((self.run / 'original-data').exists())

    def test_corrupt_backup_never_starts_old_container(self):
        self.make_backup()
        (self.run / 'backup-data/komari.db').write_bytes(b'corrupt')
        with self.assertRaises(SafeFailure):
            self.op.rollback()
        self.assertNotIn(('docker', 'start', 'old-id'), self.commands)
        self.assertNotIn(('restore-units',), self.commands)

    def test_restore_copy_failure_preserves_live_tree_and_does_not_start(self):
        self.make_backup()
        (self.data / 'komari.db').write_bytes(b'candidate')
        def fail_copy(*args):
            raise SafeFailure('simulated_disk_failure')
        self.op.copy_tree = fail_copy
        with self.assertRaises(SafeFailure):
            self.op.rollback()
        self.assertEqual((self.data / 'komari.db').read_bytes(), b'candidate')
        self.assertNotIn(('docker', 'start', 'old-id'), self.commands)

    def test_stop_new_failure_prevents_data_restore_and_old_start(self):
        self.make_backup()
        (self.data / 'komari.db').write_bytes(b'candidate')
        def fail_stop():
            raise SafeFailure('simulated_stop_failure')
        self.op.stop_owned_new_containers = fail_stop
        with self.assertRaises(SafeFailure):
            self.op.rollback()
        self.assertEqual((self.data / 'komari.db').read_bytes(), b'candidate')
        self.assertNotIn(('docker', 'start', 'old-id'), self.commands)

    def test_nginx_external_change_is_not_overwritten_during_rollback(self):
        self.make_backup()
        (self.run / 'nginx-before.conf').write_bytes(b'old nginx')
        (self.run / 'artifacts').mkdir()
        (self.run / 'artifacts/nginx-after.conf').write_bytes(b'new nginx')
        self.nginx.write_bytes(b'unrelated admin edit')
        self.op.state['nginx_intent'] = True
        with self.assertRaisesRegex(SafeFailure, 'nginx_changed_externally_during_rollback'):
            self.op.rollback()
        self.assertEqual(self.nginx.read_bytes(), b'unrelated admin edit')
        self.assertNotIn(('docker', 'start', 'old-id'), self.commands)

    def test_late_health_failure_automatically_restores_whole_original(self):
        expected = self.make_backup()
        self.op.state['swap_intent'] = False
        self.op.preflight = lambda: None
        def prepare():
            shutil.copytree(self.run / 'backup-data', self.run / 'candidate-data')
            (self.run / 'candidate-data/komari.db').write_bytes(b'migrated')
            self.op.state['candidate_ready'] = True
        self.op.migrate_and_audit = prepare
        def fail_health():
            raise SafeFailure('simulated_candidate_health_failure')
        self.op.start_candidate = fail_health
        with self.assertRaisesRegex(SafeFailure, '^upgrade_failed_rolled_back$'):
            self.op.execute()
        self.assertEqual(tree_manifest(self.data), expected)
        self.assertTrue(self.op.state['rolled_back'])
        self.assertEqual(self.op.state['failed_phase'], 'switching_data')
        self.assertTrue((self.run / 'original-data').exists())
        self.assertIn(('docker', 'start', 'old-id'), self.commands)

    def test_journal_write_failure_cannot_skip_automatic_rollback(self):
        expected = self.make_backup()
        self.op.state['swap_intent'] = False
        self.op.preflight = lambda: None
        def prepare():
            shutil.copytree(self.run / 'backup-data', self.run / 'candidate-data')
            (self.run / 'candidate-data/komari.db').write_bytes(b'migrated')
            self.op.state['candidate_ready'] = True
        self.op.migrate_and_audit = prepare
        def unavailable_journal(**changes):
            raise OSError('simulated ENOSPC on journal')
        def fail_health():
            self.op.save = unavailable_journal
            raise SafeFailure('simulated_candidate_health_failure')
        self.op.start_candidate = fail_health
        with self.assertRaisesRegex(SafeFailure, '^upgrade_failed_rolled_back$'):
            self.op.execute()
        self.assertEqual(tree_manifest(self.data), expected)
        self.assertTrue(self.op.state['rolled_back'])
        self.assertIn(('stop-new',), self.commands)
        self.assertIn(('docker', 'start', 'old-id'), self.commands)

    def test_failed_unit_stop_still_stops_containers_but_never_swaps_data(self):
        self.make_backup()
        (self.data / 'komari.db').write_bytes(b'candidate')
        def failed_stop():
            raise SafeFailure('simulated_systemctl_failure')
        self.op.stop_units = failed_stop
        with self.assertRaisesRegex(SafeFailure, '^rollback_could_not_stop_all_writers$'):
            self.op.rollback()
        self.assertIn(('stop-new',), self.commands)
        self.assertIn(('stop', 'old-id'), self.commands)
        self.assertNotIn(('docker', 'start', 'old-id'), self.commands)
        self.assertEqual((self.data / 'komari.db').read_bytes(), b'candidate')

    def test_interrupted_complete_restore_copy_is_verified_and_reused(self):
        expected = self.make_backup()
        ready = self.run / 'restore-data-1234abcd'
        shutil.copytree(self.run / 'backup-data', ready, copy_function=shutil.copy2)
        self.op.state['restore_ready'] = str(ready)
        (self.data / 'komari.db').write_bytes(b'candidate')
        def no_extra_copy(*args):
            self.fail('verified completed recovery copy should be reused')
        self.op.copy_tree = no_extra_copy
        self.op.rollback()
        self.assertEqual(tree_manifest(self.data), expected)
        self.assertFalse(ready.exists())
        self.assertIn(('docker', 'start', 'old-id'), self.commands)

    def test_only_exact_nginx_removal_is_accepted(self):
        before = b'prefix\n' + NGINX_REMOVAL.encode() + b'other-config\n'
        validate_nginx(before, b'prefix\nother-config\n')
        with self.assertRaises(SafeFailure):
            validate_nginx(before, b'prefix\nchanged-config\n')

    def test_multiline_env_rejected_without_exposing_value(self):
        secret = 'TOKEN=never-print-this\ninjected=value'
        with self.assertRaisesRegex(SafeFailure, '^unsupported_environment_value$'):
            env_file({'Config': {'Env': [secret]}})
        self.assertEqual(env_file({'Config': {'Env': ['TOKEN=a=b c']}}), b'TOKEN=a=b c\n')

    def test_links_in_backup_are_rejected(self):
        (self.data / 'outside').symlink_to(self.nginx)
        with self.assertRaisesRegex(SafeFailure, 'data_contains_link_or_special_file'):
            tree_manifest(self.data)

    def test_default_dry_run_calls_no_runner(self):
        with open(os.devnull, 'w') as output, contextlib.redirect_stdout(output):
            self.assertEqual(main([]), 0)
        self.assertEqual(self.commands, [])


def interrupted(signum, frame):
    raise SafeFailure('interrupted')


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    try:
        raise SystemExit(main())
    except SafeFailure as error:
        event('failed', code=str(error))
        raise SystemExit(1)
    except Exception:
        # Do not print traceback, exception text, subprocess output or credentials.
        event('failed', code='unexpected_error_details_suppressed')
        raise SystemExit(1)
