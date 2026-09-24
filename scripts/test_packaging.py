"""Offline packaging regression tests; fixtures are not release artifacts."""
import contextlib
import hashlib
import importlib.util
import io
import os
from pathlib import Path
import platform
import shutil
import stat
import subprocess
import tarfile
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent


def load(name):
    spec = importlib.util.spec_from_file_location(name.replace('-', '_'), HERE / (name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


installer = load('install')
release = load('release')
launcher = load('run-local')
metadata = load('release-metadata')


class PackagingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)

    def fixture(self):
        source = self.path / 'janus_2026.9.1_linux_amd64'
        source.mkdir()
        # Synthetic ELF header tests archive handling only, never execution.
        machine = {'x86_64': 62, 'aarch64': 183, 'arm64': 183}[platform.machine()]
        header = b'\x7fELF\x02\x01' + bytes(12) + machine.to_bytes(2, 'little')
        (source / 'janus').write_bytes(header)
        for name in release.LEGAL + ('BUILD', 'run-local.py'):
            (source / name).write_text('packaging test fixture\n')
        archive = self.path / (source.name + '.tar.gz')
        release.archive(source, archive, 0)
        checksums = self.checksums(archive)
        return source, archive, checksums

    def checksums(self, archive):
        path = self.path / 'SHA256SUMS'
        path.write_text(hashlib.sha256(archive.read_bytes()).hexdigest() + '  ' + archive.name + '\n')
        return path

    def test_archive_install_and_legal_files(self):
        source, archive, checksums = self.fixture()
        prefix = self.path / 'prefix with spaces'
        with contextlib.redirect_stdout(io.StringIO()):
            installer.install(archive, checksums, prefix)
            installer.install(archive, checksums, prefix)
        self.assertEqual((prefix / 'bin/janus').read_bytes(), (source / 'janus').read_bytes())
        self.assertEqual(stat.S_IMODE((prefix / 'bin/janus').stat().st_mode), 0o755)
        for name in release.LEGAL:
            self.assertEqual((prefix / 'share/janus' / name).read_bytes(), (source / name).read_bytes())

    def test_archive_reproducibility(self):
        source, archive, _ = self.fixture()
        second = self.path / 'second.tar.gz'
        release.archive(source, second, 0)
        self.assertEqual(archive.read_bytes(), second.read_bytes())

    def test_checksum_mismatch_and_missing_and_duplicate(self):
        _, archive, checksums = self.fixture()
        good = checksums.read_text()
        for content in ('', good + good, '0' * 64 + '  ' + archive.name + '\n'):
            checksums.write_text(content)
            with self.assertRaises(ValueError):
                installer.install(archive, checksums, self.path / 'prefix')
            self.assertFalse((self.path / 'prefix').exists())

    def test_archive_rejects_traversal_symlink_and_duplicates(self):
        for kind in ('traversal', 'symlink', 'duplicate'):
            archive = self.path / (kind + '.tar.gz')
            with tarfile.open(archive, 'w:gz') as tar:
                info = tarfile.TarInfo('../escape' if kind == 'traversal' else 'root/test')
                if kind == 'symlink':
                    info.type = tarfile.SYMTYPE
                    info.linkname = '/etc/passwd'
                tar.addfile(info, io.BytesIO())
                if kind == 'duplicate':
                    tar.addfile(info, io.BytesIO())
            with self.assertRaises(ValueError):
                installer.install(archive, self.checksums(archive), self.path / 'prefix')
            self.assertFalse((self.path / 'prefix').exists())

    def test_key_stability_private_permissions_and_lost_key(self):
        key = self.path / 'encryption.key'
        db = self.path / 'janus.db'
        first = launcher.encryption_key(key, db)
        self.assertEqual(len(first), 64)
        self.assertEqual(first, launcher.encryption_key(key, db))
        self.assertEqual(stat.S_IMODE(key.stat().st_mode), 0o600)
        key.chmod(0o644)
        with self.assertRaises(ValueError):
            launcher.encryption_key(key, db)
        key.unlink()
        db.touch()
        with self.assertRaises(ValueError):
            launcher.encryption_key(key, db)

    def test_compose_key_never_overwritten(self):
        output = self.path / '.env'
        command = ['python3', str(HERE / 'init-env.py'), '--output', str(output)]
        self.assertEqual(subprocess.run(command, capture_output=True).returncode, 0)
        before = output.read_bytes()
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        self.assertNotEqual(subprocess.run(command, capture_output=True).returncode, 0)
        self.assertEqual(before, output.read_bytes())

    def test_metadata_rejects_invalid_tag_or_missing_changelog(self):
        changelog = '## [2026.9.1] - 2026-09-20\n\nInitial public release.\n'
        version, date, notes = metadata.metadata('v2026.9.1', changelog, '2027-01-01')
        self.assertEqual((version, date), ('2026.9.1', '2026-09-20'))
        self.assertIn('Initial public release', notes)
        for tag in ('v2026.09.1', 'v2026.13.1', 'v2026.9.0', 'v2026.9.1;echo bad'):
            with self.assertRaises(ValueError):
                metadata.metadata(tag, changelog, '2026-09-20')
        with self.assertRaises(ValueError):
            metadata.metadata('v2026.9.2', changelog, '2026-09-20')


class ServerInstallTests(unittest.TestCase):
    """Pure helpers of the server install; systemd paths are exercised on real VMs."""

    def test_redirect_target_keeps_path_and_query(self):
        self.assertEqual(installer.redirect_target('ai.example.com', '/x?a=1', 443, 'fb'), 'https://ai.example.com/x?a=1')
        self.assertEqual(installer.redirect_target('ai.example.com:80', '/', 8443, 'fb'), 'https://ai.example.com:8443/')
        self.assertEqual(installer.redirect_target('[2001:db8::1]', '/v1', 443, 'fb'), 'https://[2001:db8::1]/v1')

    def test_redirect_target_rejects_hostile_host_and_path(self):
        self.assertEqual(installer.redirect_target('evil.com/x@y', '/', 443, 'fb.example'), 'https://fb.example/')
        self.assertEqual(installer.redirect_target('', '/', 443, 'fb.example'), 'https://fb.example/')
        self.assertEqual(installer.redirect_target('a.b', '//evil.com/', 443, 'fb'), 'https://a.b/')

    def test_acme_token_pattern(self):
        self.assertTrue(installer.TOKEN_RE.match('abc_DEF-123'))
        for bad in ('../etc/passwd', 'a/b', '', 'a.b'):
            self.assertFalse(installer.TOKEN_RE.match(bad))

    def test_hostname_and_wildcard_matching(self):
        self.assertTrue(installer.valid_hostname('ai.example.com'))
        self.assertTrue(installer.valid_hostname('10.0.0.5'))
        self.assertFalse(installer.valid_hostname('bad_host!'))
        self.assertTrue(installer.name_matches('ai.example.com', ['*.example.com']))
        self.assertFalse(installer.name_matches('a.b.example.com', ['*.example.com']))
        self.assertFalse(installer.name_matches('example.com', ['*.example.com']))

    def test_units_bind_low_ports_without_root(self):
        units = installer.unit_files(443)
        service = units['janus.service']
        self.assertIn('User=janus', service)
        self.assertIn('AmbientCapabilities=CAP_NET_BIND_SERVICE', service)
        self.assertIn('EnvironmentFile=/etc/janus/janus.env', service)
        self.assertIn('--port 80 --https-port 443', units['janus-redirect.service'])

    @unittest.skipUnless(shutil.which('openssl'), 'openssl not installed')
    def test_cert_pair_check_rejects_mismatched_key(self):
        def make(name):
            subprocess.run(['openssl', 'req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:prime256v1',
                            '-nodes', '-days', '2', '-subj', '/CN=' + name, '-addext', 'subjectAltName=DNS:' + name,
                            '-keyout', str(self.path / (name + '.key')), '-out', str(self.path / (name + '.pem'))],
                           check=True, capture_output=True)
        make('a.example.com')
        make('b.example.com')
        info = installer.check_cert_pair(self.path / 'a.example.com.pem', self.path / 'a.example.com.key')
        self.assertIn('a.example.com', info['names'])
        self.assertTrue(info['self_signed'])
        with self.assertRaises(installer.InstallError):
            installer.check_cert_pair(self.path / 'a.example.com.pem', self.path / 'b.example.com.key')

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)


if __name__ == '__main__':
    unittest.main()
