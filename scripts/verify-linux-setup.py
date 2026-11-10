#!/usr/bin/env python3
"""Verify enforcement, the durable journal, and delivery through Alloy to Grafana."""
import json
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import time
import urllib.parse
import urllib.request
import uuid

CELL = '/opt/relayna-cell/bin/cell'


def cell(*args):
    return subprocess.check_output([CELL, *args], text=True, timeout=180)


def get(url):
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(url, timeout=5) as response:
        return response.read()


def eventually(fn, seconds=180):
    deadline = time.monotonic()+seconds
    last = None
    while time.monotonic() < deadline:
        try:
            result = fn()
            if result:
                return result
        except (OSError, ValueError, subprocess.SubprocessError) as error:
            last = error
        time.sleep(2)
    raise RuntimeError('Verification timed out: '+str(last))


def main():
    eventually(lambda: cell('health'))
    config = json.loads(Path('/etc/relayna-cell/setup.json').read_text())
    marker = 'setup-'+uuid.uuid4().hex
    spec = {'name': marker, 'image': config['images']['probe'], 'mode': 'session', 'timeout_seconds': 600}
    ids = []

    def create(value):
        with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as f:
            json.dump(value, f)
            f.flush()
            result = json.loads(cell('create', '-f', f.name))
        ids.append(result['id'])
        assert result['state'] == 'running', result
        return result['id']

    try:
        id = create(spec)
        program = '''import os,socket,subprocess,resource
assert os.getuid()==65532
assert resource.getrlimit(resource.RLIMIT_NPROC)==(64,64)
assert resource.getrlimit(resource.RLIMIT_NOFILE)==(1024,1024)
assert not os.path.exists('/var/run/docker.sock')
assert not os.path.exists('/run/podman/podman.sock')
try:
 open('/etc/cell-probe','w').write('escape')
except OSError: pass
else: raise AssertionError('root filesystem is writable')
open('/workspace/probe','w').write('allowed')
subprocess.check_call(['/bin/true'])
s=socket.socket(); s.settimeout(2)
try: s.connect(('1.1.1.1',443))
except OSError: pass
else: raise AssertionError('offline Cell reached the internet')
finally: s.close()
print(%r)
''' % marker
        assert marker in cell('exec', id, '--', 'python3', '-c', program)
        events = eventually(lambda: [json.loads(x) for x in cell('events', id).splitlines() if x])
        output = next(e for e in events if e['event_kind'] == 'output' and marker in json.dumps(e['data']))
        eventually(lambda: any(e.get('source') == 'runtime' and e.get('event_kind') == 'process.exec'
                              for e in map(json.loads, cell('events', id).splitlines())))
        print('[OK] gVisor execution, unprivileged UID, read-only root, writable workspace, internet blocked', flush=True)
        with sqlite3.connect('file:/var/lib/relayna-cell/cell.db?mode=ro', uri=True) as db:
            assert db.execute('PRAGMA quick_check').fetchone()[0] == 'ok'
        print('[OK] SQLite integrity', flush=True)
        eventually(lambda: get('http://127.0.0.1:3100/ready'))
        query = '{service_name="relayna-cell"} | json | event_id="'+output['event_id']+'"'
        url = 'http://127.0.0.1:3100/loki/api/v1/query_range?'+urllib.parse.urlencode({
            'query': query, 'start': str(time.time_ns()-600*10**9), 'limit': 20})
        eventually(lambda: json.loads(get(url))['data']['result'])
        print('[OK] Exact workload event delivered through Alloy to Loki', flush=True)
        eventually(lambda: get('http://127.0.0.1:3000/api/health'))
        for uid in ('relayna-cell', 'relayna-cell-audit'):
            dashboard = json.loads(get('http://127.0.0.1:3000/api/dashboards/uid/'+uid))
            assert dashboard['dashboard']['panels']
        health = json.loads(get('http://127.0.0.1:3000/api/datasources/uid/cell-loki/health'))
        assert health['status'] == 'OK', health
        print('[OK] Grafana datasource and both provisioned dashboards', flush=True)
        if config['inference'] != 'none':
            inference = json.loads(Path('/etc/relayna-cell/inference.json').read_text())
            model_spec = json.loads(Path('/etc/relayna-cell/model-cell.json').read_text())
            mid = create(model_spec)
            program = '''import json,urllib.request,urllib.error,socket
base=%r
request=urllib.request.Request(base+'/chat/completions',data=json.dumps({'model':%r,'messages':[{'role':'user','content':'Say OK. /no_think'}],'max_tokens':32,'stream':False}).encode(),headers={'Content-Type':'application/json'})
with urllib.request.urlopen(request,timeout=120) as r: assert json.load(r)['choices']
for url in [base.replace('/v1','/api/tags'),'http://example.com/']:
 try: urllib.request.urlopen(url,timeout=5)
 except urllib.error.HTTPError as e: assert e.code==403
 else: raise AssertionError('management or internet request allowed')
s=socket.socket();s.settimeout(2)
try:s.connect(('1.1.1.1',443))
except OSError:pass
else:raise AssertionError('direct internet bypass')
finally:s.close()
print('inference-and-policy-ok')
''' % (inference['base_url'], inference['model'])
            assert 'inference-and-policy-ok' in cell('exec', '--timeout', '180', mid, '--', 'python3', '-c', program)
            print('[OK] Cell inference, model-management denial, proxied and direct internet denial', flush=True)
    finally:
        for id in ids:
            cell('delete', id)


if __name__ == '__main__':
    main()
