#!/usr/bin/env python3
"""Implementation helper for setup-linux.sh. Uses only Python's standard library."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time

import importlib.util
_spec = importlib.util.spec_from_file_location('linux_services', Path(__file__).with_name('linux-services.py'))
services = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(services)
ROOT = Path('/opt/relayna-cell')
ETC = Path('/etc/relayna-cell')
SOURCE = Path(__file__).resolve().parent.parent
GVISOR = '20261102.0'
CHECKSUMS = {
    'x86_64': '3c400f72be459b57b99b1c4f69be96bcd67eef3caf2078e595fc98e688b6b9a3af7802646b3dde7da8a9aba96d34f86be303e0430c7868ab2d13becdd403c0c1',
    'aarch64': 'e829ca32eef36f4e960460a6dcb094d7945ab507e9bfe0275f09467ab9bfeb846528fddfd42349ad64bb4f5b02148e1065c06e573feb8f6a8b3f47efa30a7d2e',
}


def run(*args, capture=False, check=True):
    return subprocess.run([str(a) for a in args], check=check, text=True,
                          stdout=subprocess.PIPE if capture else None,
                          stderr=subprocess.PIPE if capture else None)


def write(path, text, mode=0o644):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    if path.exists() and path.read_text() == text:
        return
    if path.exists():
        backup = path.with_name(path.name+'.before-setup-'+str(time.time_ns()))
        shutil.copy2(path, backup)
    temp = path.with_name(path.name+'.new')
    temp.write_text(text)
    temp.chmod(mode)
    temp.replace(path)


def file_hash(path, algorithm='sha256'):
    h = hashlib.new(algorithm)
    with open(path, 'rb') as f:
        for chunk in iter(lambda: f.read(1 << 20), b''):
            h.update(chunk)
    return h.hexdigest()


def download(url, path, digest, algorithm='sha256'):
    run('curl', '--fail', '--location', '--proto', '=https', '--tlsv1.2',
        '--retry', '3', '--connect-timeout', '20', '--output', path, url)
    if file_hash(path, algorithm) != digest:
        raise RuntimeError('Download checksum mismatch: '+str(path))


def preflight(args, engine):
    failures = []

    def test(name, condition, advice=''):
        print(('[OK]   ' if condition else '[FAIL] ')+name, flush=True)
        if not condition:
            failures.append(name+(': '+advice if advice else ''))

    test('Linux x86_64 or aarch64', platform.system() == 'Linux' and platform.machine() in CHECKSUMS)
    test('Root', os.geteuid() == 0)
    test('Python >= 3.10', sys.version_info >= (3, 10))
    version = re.match(r'(\d+)\.(\d+)', platform.release())
    test('Linux kernel >= 5.6', version is not None and tuple(map(int, version.groups())) >= (5, 6))
    test('systemd is PID 1', Path('/run/systemd/system').is_dir(), 'use a systemd Linux VM')
    controllers = Path('/sys/fs/cgroup/cgroup.controllers')
    test('cgroup v2 with CPU, memory and PIDs controllers', controllers.is_file() and
         {'cpu', 'memory', 'pids'} <= set(controllers.read_text().split()))
    required = ['bash', 'curl', 'tar', 'zstd', 'systemctl', 'flock', args.engine]
    required += ['iptables', 'ip6tables', 'dockerd'] if args.engine == 'docker' else ['nft']
    for name in required:
        test(name, shutil.which(name) is not None, 'install this prerequisite yourself')
    test('Python sqlite3 module', sqlite3.sqlite_version_info >= (3, 31))
    enforce = Path('/sys/fs/selinux/enforce')
    test('Supported host security profile', not enforce.exists() or enforce.read_text().strip() == '0',
         'SELinux-enforcing hosts are not yet supported; use a dedicated Ubuntu/Debian VM. Do not disable host security for this installer.')
    gpu = args.inference in ('vllm', 'sglang')
    mem = int(re.search(r'MemTotal:\s+(\d+)', Path('/proc/meminfo').read_text())[1]) / 1024**2
    minimum = 15 if gpu else 3.5 if args.inference != 'none' else 3
    test(f'RAM >= {minimum:g} GiB', mem >= minimum, 'increase VM RAM')
    disk = shutil.disk_usage('/var/lib').free / 1024**3
    minimum_disk = 50 if gpu else 8 if args.inference != 'none' else 6
    test(f'Free disk >= {minimum_disk} GiB', disk >= minimum_disk, 'allow additional space for larger models')
    info = None
    if shutil.which(args.engine):
        r = engine.run('info', '--format=json' if args.engine == 'podman' else '--format={{json .}}', capture=True, check=False)
        test('Local container engine responds', r.returncode == 0, 'start/configure the chosen engine')
        if r.returncode == 0:
            info = json.loads(r.stdout)
            if args.engine == 'docker':
                test('Rootful Docker + cgroup v2', info.get('CgroupVersion') == '2' and not any('rootless' in x for x in info.get('SecurityOptions', [])))
                # The runtime backend uses the DOCKER-USER chain, not Docker's experimental nftables backend.
                if shutil.which('iptables'):
                    test('Docker iptables firewall / DOCKER-USER', run('iptables', '-S', 'DOCKER-USER', capture=True, check=False).returncode == 0)
                daemon = Path('/etc/docker/daemon.json')
                if daemon.exists():
                    try:
                        json.loads(daemon.read_text())
                    except ValueError:
                        test('Valid Docker daemon.json', False)
            else:
                host = info['host']
                test('Rootful Podman + Netavark + cgroup v2', not host['security']['rootless'] and host['networkBackend'] == 'netavark' and host['cgroupVersion'] == 'v2')
    if gpu:
        for name in ('nvidia-smi', 'nvidia-ctk'):
            test(name, shutil.which(name) is not None, 'install NVIDIA driver and Container Toolkit yourself')
        if shutil.which('nvidia-smi'):
            r = run('nvidia-smi', '--query-gpu=driver_version,memory.total', '--format=csv,noheader,nounits', capture=True, check=False)
            rows = [x.split(',') for x in r.stdout.splitlines()]
            test('NVIDIA driver >= 580 and GPU VRAM >= 8 GiB', r.returncode == 0 and bool(rows) and all(int(x[0].split('.')[0]) >= 580 and int(x[1]) >= 8000 for x in rows), 'CUDA 13 profiles; larger models need more VRAM')
        if args.engine == 'docker' and info:
            test('Docker NVIDIA runtime configured', 'nvidia' in info.get('Runtimes', {}))
        elif args.engine == 'podman' and shutil.which('nvidia-ctk'):
            r = run('nvidia-ctk', 'cdi', 'list', capture=True, check=False)
            test('Podman NVIDIA CDI devices configured', r.returncode == 0 and 'nvidia.com/gpu=' in r.stdout)
    if args.bin_dir:
        machine = 183 if platform.machine() == 'aarch64' else 62
        for name in ('cell', 'cell-init', 'cell-server'):
            path = Path(args.bin_dir)/name
            head = path.read_bytes()[:20] if path.is_file() else b''
            test('Native Linux binary: '+name, head[:4] == b'\x7fELF' and len(head) >= 20 and int.from_bytes(head[18:20], 'little') == machine)
    if args.inference == 'llama.cpp' and args.model:
        path = Path(args.model)
        with path.open('rb') if path.is_file() else open(os.devnull, 'rb') as f:
            test('Readable GGUF model', f.read(4) == b'GGUF', '--model must be a local GGUF path for llama.cpp')
    config = ETC/'setup.json'
    if config.exists():
        old = json.loads(config.read_text())
        test('Same engine as existing installation', old['engine'] == args.engine,
             'engine migration is not supported; use a fresh VM')
    elif (ETC/'install-owner.json').exists():
        test('Same engine as interrupted installation', json.loads((ETC/'install-owner.json').read_text()) == {'engine': args.engine, 'owner': 'linux-v1'})
    elif ROOT.exists() or Path('/var/lib/relayna-cell/cell.db').exists():
        test('Fresh or installer-managed installation', False, 'existing legacy installation detected; use a separate Linux VM')
    else:
        for port in (3000, 3100, 3128, 18080):
            with socket.socket() as sock:
                try:
                    sock.bind(('0.0.0.0', port))
                    available = True
                except OSError:
                    available = False
                test(f'TCP port {port} available', available)
    if failures:
        print('\nPrerequisites are not green. No images or software were installed.\n'+
              '\n'.join('  - '+x for x in failures)+
              '\nInstall/configure the missing infrastructure yourself, then rerun.\n'
              'Docker: https://docs.docker.com/engine/install/\n'
              'Podman: https://podman.io/docs/installation\n'
              'NVIDIA: https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html')
        raise SystemExit(1)


def install(args, engine):
    import fcntl
    lock = open('/run/relayna-cell-setup.lock', 'w')
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    images = json.loads((SOURCE/'deploy/linux/images.json').read_text())
    chosen = ['loki', 'alloy', 'grafana', 'probe']
    if not args.bin_dir:
        chosen.append('builder')
    if args.inference != 'none':
        chosen += [args.inference, 'gateway']
    print('\nAll prerequisites green. Pulling images and staging verified binaries.', flush=True)
    for name in chosen:
        engine.run('pull', images[name])
        # Persist immutable references for boot/reinstall diagnostics.
        obj = json.loads(engine.run('image', 'inspect', images[name], capture=True).stdout)[0]
        digests = obj.get('RepoDigests') or []
        if digests:
            images[name] = digests[0]
    with tempfile.TemporaryDirectory(prefix='relayna-setup-') as temp:
        stage = Path(temp)
        binaries = Path(args.bin_dir).resolve() if args.bin_dir else stage/'bin'
        if not args.bin_dir:
            binaries.mkdir()
            engine.run('run', '--rm', '-e', 'CGO_ENABLED=0', '-e', 'GOTOOLCHAIN=local',
                       '-v', str(SOURCE)+':/src:ro', '-v', str(binaries)+':/out', '-w', '/src',
                       images['builder'], 'sh', '-ec',
                       'for name in cell cell-init cell-server; do go build -trimpath -o /out/$name ./cmd/$name; done')
        archive = stage/'gvisor.tar.zstd'
        download(f'https://storage.googleapis.com/gvisor/releases/release/{GVISOR}/{platform.machine()}/gvisor.tar.zstd',
                 archive, CHECKSUMS[platform.machine()], 'sha512')
        runtime = stage/'runtime'
        runtime.mkdir()
        run('tar', '--zstd', '-xf', archive, '-C', runtime)
        run(runtime/'runsc', '--version')
        # Downloads/builds complete before touching an existing installation.
        if (ETC/'setup.json').exists():
            print('Updating managed installation: stopping Cells and infrastructure; persistent data is retained.', flush=True)
            for unit in ('cell-server.service', 'relayna-infrastructure.service'):
                if run('systemctl', 'show', '--property=LoadState', '--value', unit, capture=True).stdout.strip() == 'loaded':
                    run('systemctl', 'stop', unit)
        write(ETC/'install-owner.json', json.dumps({'engine': args.engine, 'owner': 'linux-v1'})+'\n', 0o600)
        ROOT.mkdir(parents=True, exist_ok=True)
        ETC.mkdir(parents=True, exist_ok=True)
        for directory in ('scripts', 'deploy'):
            if SOURCE/directory != ROOT/directory:
                shutil.copytree(SOURCE/directory, ROOT/directory, dirs_exist_ok=True,
                                ignore=shutil.ignore_patterns('._*', '__pycache__', '*.pyc'))
        for name in ('cell', 'cell-init', 'cell-server'):
            target = ROOT/'bin'/name
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(binaries/name, target.with_suffix('.new'))
            target.with_suffix('.new').chmod(0o755)
            target.with_suffix('.new').replace(target)
        runtime_target = ROOT/'runtime'/GVISOR
        if not runtime_target.exists():
            shutil.copytree(runtime, runtime_target)
        write(ROOT/'runtime/runsc-cell', '#!/bin/sh\nexec '+str(runtime_target/'runsc')+
              ' --platform=systrap --pod-init-config=/etc/relayna-cell/pod-init.json "$@"\n', 0o755)
        write(ETC/'pod-init.json', (SOURCE/'deploy/runtime/pod-init.json').read_text())
        if args.engine == 'docker':
            path = Path('/etc/docker/daemon.json')
            data = json.loads(path.read_text()) if path.exists() else {}
            data.setdefault('runtimes', {})['runsc-cell'] = {'path': str(ROOT/'runtime/runsc-cell')}
            candidate = stage/'daemon.json'
            candidate.write_text(json.dumps(data, indent=2)+'\n')
            run('dockerd', '--validate', '--config-file', candidate)
            write(path, candidate.read_text(), path.stat().st_mode & 0o777 if path.exists() else 0o644)
            run('systemctl', 'reload', 'docker')
        model = args.model or ('qwen3:0.6b' if args.inference == 'ollama' else
                              'Qwen3-0.6B-Q8_0' if args.inference == 'llama.cpp' else 'Qwen/Qwen3-0.6B')
        if args.inference == 'llama.cpp':
            models = Path('/var/lib/relayna-cell-models')
            models.mkdir(exist_ok=True)
            if args.model:
                shutil.copyfile(Path(args.model).resolve(), models/'model.gguf.new')
                model = Path(args.model).stem
                (models/'model.gguf.new').replace(models/'model.gguf')
            else:
                expected = '9465e63a22add5354d9bb4b99e90117043c7124007664907259bd16d043bb031'
                if not (models/'model.gguf').exists() or file_hash(models/'model.gguf') != expected:
                    download('https://huggingface.co/Qwen/Qwen3-0.6B-GGUF/resolve/23749fefcc72300e3a2ad315e1317431b06b590a/Qwen3-0.6B-Q8_0.gguf',
                             models/'model.gguf.new', expected)
                    (models/'model.gguf.new').replace(models/'model.gguf')
        config = {'version': 1, 'engine': args.engine, 'inference': args.inference, 'model': model, 'images': images}
        write(ETC/'setup.json', json.dumps(config, indent=2)+'\n', 0o600)
        data = Path('/var/lib/relayna-cell')
        data.mkdir(mode=0o700, exist_ok=True)
        (data/'logs').mkdir(mode=0o755, exist_ok=True)
        dependency = 'Requires=docker.service\nAfter=docker.service\nPartOf=docker.service\n' if args.engine == 'docker' else ''
        write('/etc/systemd/system/cell-server.service', '[Unit]\nDescription=Relayna Cell sandbox runtime\n'+dependency+'''
[Service]
Type=simple
User=root
UMask=0077
RuntimeDirectory=relayna-cell
RuntimeDirectoryMode=0700
ExecStartPre=/bin/bash /opt/relayna-cell/scripts/network-guard.sh --engine '''+args.engine+'''
ExecStart=/opt/relayna-cell/bin/cell-server --engine '''+args.engine+'''
Restart=on-failure
RestartSec=3
TimeoutStopSec=30
LimitNOFILE=8192
[Install]
WantedBy=multi-user.target
''')
        write('/etc/systemd/system/relayna-infrastructure.service', '[Unit]\nDescription=Relayna Cell observability and inference\nWants=network-online.target\nAfter=network-online.target\n'+dependency+'''
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/python3 /opt/relayna-cell/scripts/linux-services.py up --load-model
ExecStop=/usr/bin/python3 /opt/relayna-cell/scripts/linux-services.py down
TimeoutStartSec=1200
TimeoutStopSec=150
[Install]
WantedBy=multi-user.target
''')
        write('/usr/local/bin/cell', '#!/bin/sh\nexec /opt/relayna-cell/bin/cell "$@"\n', 0o755)
        run('systemctl', 'daemon-reload')
        run('systemctl', 'enable', 'cell-server.service', 'relayna-infrastructure.service')
        run('systemctl', 'restart', 'cell-server.service')
        run('systemctl', 'start', 'relayna-infrastructure.service')
        run('python3', ROOT/'scripts/verify-linux-setup.py')
    print('\nRelayna Cell is ready.\n  Health: sudo cell health\n'
          '  Grafana: http://127.0.0.1:3000/d/relayna-cell\n'
          '  Audit dashboard: http://127.0.0.1:3000/d/relayna-cell-audit\n'
          '  Remote access: ssh -L 3000:127.0.0.1:3000 USER@LINUX_HOST\n'
          '  Settings: /etc/relayna-cell/setup.json\n'
          '  Services: cell-server.service, relayna-infrastructure.service')
    if args.inference != 'none':
        print('  Model endpoint/settings: /etc/relayna-cell/inference.json\n'
              '  Create a model-enabled Cell: sudo cell create -f /etc/relayna-cell/model-cell.json')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--engine', choices=['docker', 'podman'], required=True)
    parser.add_argument('--inference', choices=['none', 'ollama', 'llama.cpp', 'vllm', 'sglang'], required=True)
    parser.add_argument('--model', default='')
    parser.add_argument('--bin-dir', default='')
    parser.add_argument('--check', choices=['0', '1'], default='0')
    args = parser.parse_args()
    try:
        engine = services.Engine(args.engine)
        preflight(args, engine)
        if args.check == '0':
            install(args, engine)
        else:
            print('\nAll prerequisites green. Check only: nothing installed.')
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError) as error:
        print('Setup failed: '+str(error), file=sys.stderr)
        print('Inspect: journalctl -u cell-server -u relayna-infrastructure --no-pager -n 100', file=sys.stderr)
        raise SystemExit(1)
