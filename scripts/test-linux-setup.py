#!/usr/bin/env python3
"""Host-safe installer regression checks; real VM checks live in verify-linux-setup.py."""
import importlib.util
import json
import os
from pathlib import Path
import pty
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SCRIPTS = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('setup', SCRIPTS/'linux-setup.py')
setup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(setup)


class SetupTests(unittest.TestCase):
    def cli(self, *args):
        return subprocess.run(['bash', str(SCRIPTS/'setup-linux.sh'), *args], input='', text=True,
                              stdout=subprocess.PIPE, stderr=subprocess.STDOUT)

    def test_noninteractive_requires_both_choices(self):
        for args in ((), ('--engine', 'docker'), ('--inference', 'ollama')):
            result = self.cli(*args)
            self.assertEqual(result.returncode, 2)
            self.assertIn('--engine and --inference', result.stdout)

    def test_invalid_selection_never_falls_back(self):
        for args in (('--engine', 'unknown', '--inference', 'none'),
                     ('--engine', 'docker', '--inference', 'unknown')):
            self.assertEqual(self.cli(*args).returncode, 2)

    def test_help_lists_all_choices(self):
        result = self.cli('--help')
        self.assertEqual(result.returncode, 0)
        for value in ('docker', 'podman', 'ollama', 'llama.cpp', 'vllm', 'sglang', '--check'):
            self.assertIn(value, result.stdout)

    def test_interactive_choice_order(self):
        master, slave = pty.openpty()
        try:
            process = subprocess.Popen(['bash', str(SCRIPTS/'setup-linux.sh'), '--check'],
                                       stdin=slave, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            os.close(slave)
            os.write(master, b'2\n2\n\n')
            output = process.communicate(timeout=30)[0]
            self.assertLess(output.index('Container engine:'), output.index('Inference engine:'))
            self.assertLess(output.index('Inference engine:'), output.index('Model ['))
        finally:
            os.close(master)

    def test_hash_mismatch_aborts(self):
        with tempfile.TemporaryDirectory() as tmp:
            destination = Path(tmp)/'archive'
            def fake_download(*args):
                destination.write_bytes(b'corrupt download')
            with patch.object(setup, 'run', side_effect=fake_download):
                with self.assertRaisesRegex(RuntimeError, 'checksum mismatch'):
                    setup.download('https://example.invalid/archive', destination, '0'*64)

    def test_foreign_named_resources_are_rejected(self):
        for engine in ('docker', 'podman'):
            e = setup.services.Engine(engine)
            result = subprocess.CompletedProcess([], 0, json.dumps([{'Config': {'Labels': {}}}]), '')
            with patch.object(e, 'run', return_value=result) as command:
                with self.assertRaisesRegex(RuntimeError, 'not owned'):
                    e.container('relayna-linux-loki', [], 'image')
                self.assertEqual(command.call_count, 1)  # only inspect, no removal

    def test_changed_config_is_backed_up(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)/'config.json'
            path.write_text('{"keep":true}\n')
            setup.write(path, '{"keep":true,"runtime":"runsc-cell"}\n')
            backups = list(Path(tmp).glob('config.json.before-setup-*'))
            self.assertEqual(len(backups), 1)
            self.assertEqual(json.loads(backups[0].read_text()), {'keep': True})
            setup.write(path, path.read_text())
            self.assertEqual(len(list(Path(tmp).glob('*.before-setup-*'))), 1)

    def test_skip_inference_removes_old_containers_but_keeps_volumes(self):
        with tempfile.TemporaryDirectory() as tmp:
            config_path = Path(tmp)/'setup.json'
            for name in ('model-cell.json', 'inference.json'):
                config_path.with_name(name).write_text('{}')
            with patch.object(setup.services, 'Engine') as factory, patch.object(setup.services, 'CONFIG', config_path):
                e = factory.return_value
                e.inspect.return_value = {'owned': True}
                setup.services.start({'engine': 'docker', 'inference': 'none',
                                      'images': dict.fromkeys(('loki', 'alloy', 'grafana'), 'image')})
                self.assertEqual([call.args for call in e.run.call_args_list], [
                    ('rm', '--force', 'relayna-linux-gateway'),
                    ('rm', '--force', 'relayna-linux-inference')])
                self.assertFalse(config_path.with_name('inference.json').exists())
                self.assertFalse(config_path.with_name('model-cell.json').exists())


if __name__ == '__main__':
    unittest.main()
