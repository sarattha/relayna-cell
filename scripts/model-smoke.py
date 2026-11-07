"""Real Ollama + OpenAI Agents SDK integration. Run as root inside the VM."""
import argparse
import http.client
import json
import pathlib
import socket
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

IMAGE = 'relayna-cell/agents-sdk:dev'
COMPOSE = ['docker', 'compose', '-f', '/opt/relayna-cell/deploy/inference/compose.yaml']
FRONTEND = 'relayna-cell-inference_frontend'


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(240)
        self.sock.connect('/run/relayna-cell/api.sock')


def api(method, path, body=None, raw=False):
    connection = UnixHTTP('cell')
    connection.request(method, path, None if body is None else json.dumps(body).encode(),
                       {'Content-Type': 'application/json'})
    response = connection.getresponse()
    data = response.read()
    connection.close()
    if response.status >= 300:
        raise RuntimeError((response.status, data.decode(errors='replace')))
    return data if raw else json.loads(data)


def events(cell_id):
    result, cursor = [], 0
    while True:
        page = api('GET', f'/v1alpha1/events?cell_id={cell_id}&after={cursor}')
        if not page:
            return result
        result.extend(page)
        cursor = page[-1]['sequence']


def logs(cell_id):
    return ''.join(e['data']['text'] for e in events(cell_id)
                   if e['event_kind'] == 'output' and e['source'] == 'workload')


def wait_finished(cell_id):
    deadline = time.monotonic() + 220
    while time.monotonic() < deadline:
        c = api('GET', f'/v1alpha1/cells/{cell_id}')
        if c['state'] not in ('preparing', 'running'):
            # Lifecycle state may be visible just before final log persistence.
            time.sleep(.3)
            return c
        time.sleep(.5)
    raise RuntimeError('agent did not finish before deadline')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--mode', choices=['both', 'contained', 'delegated'], default='both')
    parser.add_argument('--model', default='qwen3:4b-instruct')
    args = parser.parse_args()
    subprocess.run(['docker', 'image', 'inspect', IMAGE], check=True, stdout=subprocess.DEVNULL)
    gateway_id = subprocess.check_output(COMPOSE + ['ps', '-q', 'gateway'], text=True).strip()
    if not gateway_id:
        raise RuntimeError('Run make inference-up first')
    gateway = json.loads(subprocess.check_output(['docker', 'inspect', gateway_id]))[0]
    ip = gateway['NetworkSettings']['Networks'][FRONTEND]['IPAddress']
    endpoint = f'http://{ip}:8080'
    with urllib.request.urlopen(endpoint+'/healthz', timeout=5) as response:
        assert response.status == 200
    try:
        urllib.request.urlopen(urllib.request.Request(endpoint+'/api/delete', data=b'{}', method='DELETE'), timeout=5)
    except urllib.error.HTTPError as error:
        assert error.code == 403
    else:
        raise AssertionError('Ollama management API was exposed')
    print('PASS inference gateway health and management API restriction', flush=True)

    created, summaries = [], []
    try:
        for mode in ('delegated', 'contained'):
            if args.mode not in ('both', mode):
                continue
            command = ['python', '/app/agent.py', '--mode', mode, '--model', args.model,
                       '--endpoint', endpoint+'/v1', '--verify-demo']
            spec = {'name': 'openai-sdk-'+mode, 'image': IMAGE, 'timeout_seconds': 240,
                    'memory_mib': 512, 'workspace_mib': 64, 'pids': 64,
                    'mode': 'agent' if mode == 'contained' else 'session'}
            if mode == 'contained':
                spec['command'] = command
                spec['allow'] = [{'host': ip, 'port': 8080, 'allow_private': True}]
            c = api('POST', '/v1alpha1/cells', spec)
            created.append(c['id'])
            print(f"Running {mode} SDK agent; Cell {c['id']}", flush=True)
            if mode == 'delegated':
                # Only this trusted external orchestrator receives the local API socket.
                # The model's shell function submits commands to Cell; it never runs them here.
                name = 'cell-sdk-test-'+uuid.uuid4().hex[:12]
                try:
                    result = subprocess.run([
                        'docker', 'run', '--rm', '--name', name, '--network', FRONTEND,
                        '--read-only', '--cap-drop=ALL', '--security-opt=no-new-privileges:true',
                        '--memory=512m', '--cpus=1', '--pids-limit=64',
                        '--tmpfs=/tmp:rw,nosuid,nodev,size=16m',
                        '--mount=type=bind,src=/run/relayna-cell/api.sock,dst=/run/relayna-cell/api.sock',
                        IMAGE, *command, '--cell-id', c['id'],
                    ], capture_output=True, text=True, timeout=210)
                    output = result.stdout
                    print(output, end='', flush=True)
                    if result.returncode:
                        raise RuntimeError(result.stderr)
                finally:
                    subprocess.run(['docker', 'rm', '-f', name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                artifact = api('GET', f"/v1alpha1/cells/{c['id']}/files?path=add.py", raw=True)
                print('Generated add.py:', artifact.decode(), flush=True)
            else:
                ended = wait_finished(c['id'])
                output = logs(c['id'])
                print(output, end='', flush=True)
                if ended.get('exit_code') != 0 or ended['state'] != 'exited':
                    raise RuntimeError(ended)
            records = [json.loads(line) for line in output.splitlines() if line.startswith('{')]
            result = next(r for r in records if r.get('event') == 'agent.result')
            assert result['tool_calls'] > 0
            assert any(r.get('event') == 'agent.verified' for r in records)
            evidence = events(c['id'])
            assert any(e['event_kind'] == 'process.exec' for e in evidence)
            if mode == 'contained':
                assert any(e['event_kind'] == 'network.allow' for e in evidence)
            summaries.append({'mode': mode, 'cell_id': c['id'], **result})
            print(f'PASS {mode}: real model tool call, generated implementation, independent assertions, process audit', flush=True)

        # Verify an actual SDK run reached Loki through Alloy.
        cell_id = summaries[-1]['cell_id']
        marker = 'agent.verified' if summaries[-1]['mode'] == 'contained' else 'CELL_MODEL_VERIFIED'
        query = '{service_name="relayna-cell"} |= "'+cell_id+'" |= "'+marker+'"'
        url = 'http://127.0.0.1:3100/loki/api/v1/query_range?' + urllib.parse.urlencode({'query': query, 'since': '1h', 'limit': 100})
        for _ in range(30):
            with urllib.request.urlopen(url, timeout=5) as response:
                found = json.load(response)['data']['result']
            if found:
                break
            time.sleep(1)
        else:
            raise AssertionError('SDK execution evidence did not reach Loki')
        print('PASS real-model execution evidence in Loki', flush=True)
        report = {'model': args.model, 'framework': 'openai-agents==0.24.0', 'runs': summaries}
        destination = pathlib.Path('/var/lib/relayna-cell/model-smoke-latest.json')
        destination.write_text(json.dumps(report, indent=2)+'\n')
        print('ALL REAL-MODEL CHECKS PASSED', flush=True)
    finally:
        for cell_id in created:
            api('DELETE', '/v1alpha1/cells/'+cell_id)


if __name__ == '__main__':
    main()
