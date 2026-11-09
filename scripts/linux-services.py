#!/usr/bin/env python3
"""Start the owned infrastructure with either local engine; no Compose dependency."""
import json
import os
from pathlib import Path
import subprocess
import sys
import urllib.request
import time

ROOT = Path('/opt/relayna-cell')
CONFIG = Path('/etc/relayna-cell/setup.json')
OWNER = 'dev.relayna.installation'


class Engine:
    def __init__(self, name):
        self.name = name
        self.prefix = (['docker', '--host=unix:///var/run/docker.sock'] if name == 'docker'
                       else ['podman', '--remote=false'])

    def run(self, *args, capture=False, check=True):
        return subprocess.run(self.prefix + list(args), check=check, text=True,
                              stdout=subprocess.PIPE if capture else None,
                              stderr=subprocess.PIPE if capture else None)

    def inspect(self, kind, name):
        r = self.run(kind, 'inspect', name, capture=True, check=False)
        if r.returncode:
            # Only absence is acceptable; don't hide an unavailable daemon.
            self.run('info', capture=True)
            return None
        obj = json.loads(r.stdout)[0]
        labels = obj.get('Labels') or obj.get('labels') or obj.get('Config', {}).get('Labels') or {}
        if labels.get(OWNER) != 'linux-v1':
            raise RuntimeError(f'{kind} {name} exists but is not owned by this installer')
        return obj

    def network(self, name):
        if self.inspect('network', name) is None:
            self.run('network', 'create', '--label', OWNER+'=linux-v1', name)

    def volume(self, name, uid=0):
        if self.inspect('volume', name) is None:
            self.run('volume', 'create', '--label', OWNER+'=linux-v1', name)
        p = self.inspect('volume', name)['Mountpoint']
        os.chown(p, uid, uid)
        os.chmod(p, 0o750)

    def container(self, name, args, image, command=()):
        old = self.inspect('container', name)
        if old:
            self.run('rm', '--force', name)
        logging = (['--log-driver=local', '--log-opt=max-size=4m', '--log-opt=max-file=2']
                   if self.name == 'docker' else ['--log-driver=k8s-file', '--log-opt=max-size=4m'])
        self.run('run', '-d', '--name', name, '--label', OWNER+'=linux-v1',
                 '--restart=unless-stopped', *logging, *args, image, *command)

    def ip(self, name, network):
        return self.inspect('container', name)['NetworkSettings']['Networks'][network]['IPAddress']


def wait_http(url, seconds=180):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(url, timeout=5) as r:
                if r.status == 200:
                    return r.read()
        except (OSError, ValueError):
            pass
        time.sleep(2)
    raise RuntimeError('Readiness timed out: '+url)


def start(config, load_model=False):
    e = Engine(config['engine'])
    images = config['images']
    obs = ROOT / 'deploy/observability'
    net = 'relayna-linux-observability'
    e.network(net)
    for name, uid in [('loki', 10001), ('alloy', 0), ('grafana', 472)]:
        e.volume('relayna-linux-'+name, uid)
    e.container('relayna-linux-loki', ['--network='+net, '--network-alias=loki',
        '--memory=768m', '--cpus=1', '-p', '127.0.0.1:3100:3100',
        '-v', str(obs/'loki.yaml')+':/etc/loki/config.yaml:ro',
        '-v', 'relayna-linux-loki:/loki'], images['loki'], ['-config.file=/etc/loki/config.yaml'])
    e.container('relayna-linux-alloy', ['--network='+net, '--memory=256m', '--cpus=0.5',
        '-v', str(obs/'config.alloy')+':/etc/alloy/config.alloy:ro',
        '-v', '/var/lib/relayna-cell/logs:/var/log/relayna-cell:ro',
        '-v', 'relayna-linux-alloy:/var/lib/alloy'], images['alloy'],
        ['run', '--storage.path=/var/lib/alloy', '/etc/alloy/config.alloy'])
    env = {'GF_AUTH_ANONYMOUS_ENABLED': 'true', 'GF_AUTH_ANONYMOUS_ORG_ROLE': 'Viewer',
           'GF_AUTH_DISABLE_LOGIN_FORM': 'true', 'GF_AUTH_BASIC_ENABLED': 'false',
           'GF_SECURITY_DISABLE_INITIAL_ADMIN_CREATION': 'true', 'GF_USERS_ALLOW_SIGN_UP': 'false',
           'GF_ANALYTICS_REPORTING_ENABLED': 'false', 'GF_ANALYTICS_CHECK_FOR_UPDATES': 'false',
           'GF_SERVER_ROOT_URL': 'http://localhost:3000'}
    e.container('relayna-linux-grafana', ['--network='+net, '--memory=384m', '--cpus=0.5',
        '-p', '127.0.0.1:3000:3000', '-v', 'relayna-linux-grafana:/var/lib/grafana',
        '-v', str(obs/'grafana/provisioning')+':/etc/grafana/provisioning:ro',
        '-v', str(obs/'grafana/dashboards')+':/var/lib/grafana/dashboards:ro',
        *[a for k,v in env.items() for a in ('-e', k+'='+v)]], images['grafana'])
    kind = config['inference']
    if kind == 'none':
        for name in ('relayna-linux-gateway', 'relayna-linux-inference'):
            if e.inspect('container', name):
                e.run('rm', '--force', name)
        for name in ('model-cell.json', 'inference.json'):
            CONFIG.with_name(name).unlink(missing_ok=True)
        return
    net = 'relayna-linux-inference'
    e.network(net)
    e.volume('relayna-linux-models')
    args = ['--network='+net, '--memory='+('12g' if kind in ('vllm', 'sglang') else '2g'),
            '--cpus=2', '--shm-size=1g']
    model = config['model']
    if kind == 'ollama':
        port = 11434
        args += ['-v', 'relayna-linux-models:/root/.ollama', '-e', 'OLLAMA_NO_CLOUD=1',
                 '-e', 'OLLAMA_CONTEXT_LENGTH=4096', '-e', 'OLLAMA_NUM_PARALLEL=1',
                 '-e', 'OLLAMA_MAX_LOADED_MODELS=1', '-e', 'OLLAMA_MAX_QUEUE=4']
        command = ['serve']
    elif kind == 'llama.cpp':
        port = 8080
        args += ['-v', '/var/lib/relayna-cell-models:/models:ro']
        command = ['-m', '/models/model.gguf', '--alias', model, '--host', '0.0.0.0',
                   '--port', '8080', '-c', '4096', '--parallel', '1', '--jinja']
    else:
        port = 8000
        args += (['--gpus=all'] if e.name == 'docker' else ['--device=nvidia.com/gpu=all'])
        args += ['-v', 'relayna-linux-models:/root/.cache/huggingface']
        if kind == 'vllm':
            command = ['--model', model, '--host', '0.0.0.0', '--port', '8000',
                       '--max-model-len', '4096', '--gpu-memory-utilization', '0.8',
                       '--enable-auto-tool-choice', '--tool-call-parser', 'hermes']
        else:
            args += ['--entrypoint=python3']
            command = ['-m', 'sglang.launch_server', '--model-path', model,
                       '--host', '0.0.0.0', '--port', '8000', '--context-length', '4096',
                       '--mem-fraction-static', '0.8', '--tool-call-parser', 'qwen25']
    e.container('relayna-linux-inference', args, images[kind], command)
    backend = e.ip('relayna-linux-inference', net)
    if kind == 'ollama':
        wait_http(f'http://{backend}:{port}/api/tags')
        if load_model and e.run('exec', 'relayna-linux-inference', 'ollama', 'show', model, capture=True, check=False).returncode:
            e.run('exec', 'relayna-linux-inference', 'ollama', 'pull', model)
    else:
        wait_http(f'http://{backend}:{port}/v1/models', seconds=900)
    # Resolve upstream on the host, so both Netavark and Docker work. This file
    # is regenerated whenever infrastructure is started, before nginx starts.
    template = (ROOT/'deploy/inference/nginx.conf').read_text()
    template = template.replace('resolver 127.0.0.11 valid=10s ipv6=off;', '')
    template = template.replace('http://ollama:11434', f'http://{backend}:{port}')
    template = template.replace('Host ollama:11434', f'Host {backend}:{port}')
    Path('/etc/relayna-cell/inference-nginx.conf').write_text(template)
    e.container('relayna-linux-gateway', ['--network='+net, '--memory=64m', '--cpus=0.5',
        '--user=101:101', '--read-only', '--cap-drop=ALL', '--security-opt=no-new-privileges',
        '--tmpfs=/tmp:rw,nosuid,nodev,noexec,size=16m', '-p', '127.0.0.1:18080:8080',
        '-v', '/etc/relayna-cell/inference-nginx.conf:/etc/nginx/nginx.conf:ro'], images['gateway'])
    gateway = e.ip('relayna-linux-gateway', net)
    example = {'name': 'local-model', 'image': images['probe'], 'mode': 'session',
               'memory_mib': 512, 'timeout_seconds': 600,
               'allow': [{'host': gateway, 'port': 8080, 'allow_private': True}]}
    Path('/etc/relayna-cell/model-cell.json').write_text(json.dumps(example, indent=2)+'\n')
    Path('/etc/relayna-cell/inference.json').write_text(json.dumps({
        'engine': kind, 'model': model, 'base_url': f'http://{gateway}:8080/v1',
        'host_base_url': 'http://127.0.0.1:18080/v1'}, indent=2)+'\n')


def stop(config):
    e = Engine(config['engine'])
    for name in ('gateway', 'inference', 'grafana', 'alloy', 'loki'):
        full = 'relayna-linux-'+name
        if e.inspect('container', full):
            e.run('stop', '--time=20', full)


if __name__ == '__main__':
    config = json.loads(CONFIG.read_text())
    if sys.argv[1] == 'up':
        start(config, load_model='--load-model' in sys.argv)
    elif sys.argv[1] == 'down':
        stop(config)
    else:
        raise SystemExit('Usage: linux-services.py up [--load-model] | down')
