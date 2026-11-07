"""Integration checks against real Docker/runsc/Alloy/Loki in the dedicated VM.

The inference fixture intentionally returns a deterministic tool call. It does
not claim to test model quality or require downloading a model onto the Mac.
"""
import base64
import http.client
import http.server
import json
import pathlib
import socket
import subprocess
import threading
import time
import urllib.parse
import urllib.request

ROOT = pathlib.Path('/opt/relayna-cell')
SOCKET = '/run/relayna-cell/api.sock'

class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(150)
        self.sock.connect(SOCKET)

def api(method, path, body=None, raw=False, expected=200):
    connection = UnixHTTP('cell')
    data = body if isinstance(body, bytes) else json.dumps(body).encode() if body is not None else None
    connection.request(method, path, data, {'Content-Type': 'application/json'})
    response = connection.getresponse()
    result = response.read()
    connection.close()
    assert response.status == expected, (response.status, result.decode(errors='replace'))
    return result if raw else json.loads(result)

cells = []
def create(**overrides):
    spec = {'image': 'python:3.13-slim', 'mode': 'session', 'timeout_seconds': 120}
    spec.update(overrides)
    c = api('POST', '/v1alpha1/cells', spec, expected=201)
    cells.append(c['id'])
    return c

def execute(c, command, timeout=15):
    data = api('POST', f"/v1alpha1/cells/{c['id']}/exec", {'command': command, 'timeout_seconds': timeout}, raw=True)
    frames = [json.loads(line) for line in data.splitlines()]
    errors = [f['error'] for f in frames if f.get('error')]
    out = b''.join(base64.b64decode(f.get('data', '')) for f in frames if f['stream'] == 'stdout')
    err = b''.join(base64.b64decode(f.get('data', '')) for f in frames if f['stream'] == 'stderr')
    code = next((f['exit_code'] for f in frames if 'exit_code' in f), None)
    return code, out, err, errors

def finished(c):
    for _ in range(80):
        result = api('GET', '/v1alpha1/cells/' + c['id'])
        if result['state'] not in ('preparing', 'running'):
            return result
        time.sleep(.1)
    raise AssertionError('Cell did not finish')

def events(c):
    return api('GET', '/v1alpha1/events?cell_id=' + c['id'])

def query_loki(query):
    url = 'http://127.0.0.1:3100/loki/api/v1/query_range?' + urllib.parse.urlencode({'query': query, 'since': '1h', 'limit': 1000})
    with urllib.request.urlopen(url, timeout=10) as response:
        return json.load(response)['data']['result']

class ModelFixture(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers['Content-Length']))
        body = json.dumps({'choices': [{'message': {'tool_calls': [{'function': {'name': 'shell', 'arguments': json.dumps({'command': 'printf model-tool-ok'})}}]}}]}).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *_):
        pass

fixture = http.server.ThreadingHTTPServer(('0.0.0.0', 18081), ModelFixture)
threading.Thread(target=fixture.serve_forever, daemon=True).start()
host_ip = subprocess.check_output(['hostname', '-I'], text=True).split()[0]

try:
    assert api('GET', '/healthz')['status'] == 'ok'
    c = create()
    assert execute(c, ['sh', '-c', 'printf persistent > /workspace/test.txt'])[0] == 0
    assert execute(c, ['cat', '/workspace/test.txt'])[1] == b'persistent'
    path = f"/v1alpha1/cells/{c['id']}/files?path=input.txt"
    api('PUT', path, b'uploaded')
    assert api('GET', path, raw=True) == b'uploaded'
    assert execute(c, ['sh', '-c', 'echo forbidden > /etc/cell-test'])[0] != 0
    assert execute(c, ['sh', '-c', 'test ! -e /var/run/docker.sock'])[0] == 0
    execute(c, ['ln', '-s', '/etc/passwd', '/workspace/escape'])
    api('GET', f"/v1alpha1/cells/{c['id']}/files?path=escape", expected=422)
    config = json.loads(subprocess.check_output(['docker', 'inspect', c['container_id']]))[0]
    assert config['HostConfig']['Runtime'] == 'runsc-cell'
    assert config['HostConfig']['ReadonlyRootfs'] is True
    assert config['HostConfig']['Memory'] == 256 * 1024 * 1024
    assert config['HostConfig']['PidsLimit'] == 64 + 128
    assert {'Name': 'nproc', 'Soft': 64, 'Hard': 64} in config['HostConfig']['Ulimits']
    assert config['HostConfig']['NanoCpus'] == 1_000_000_000
    print('PASS delegated tools, workspace transfer, filesystem and runtime profile')

    netcell = create(allow=[{'host': host_ip, 'port': 18081, 'allow_private': True}])
    code, out, err, errors = execute(netcell, ['python', '-c', f"import urllib.request; print(urllib.request.urlopen(urllib.request.Request('http://{host_ip}:18081/v1/chat/completions',data=b'{{}}'),timeout=5).status)"])
    assert code == 0 and b'200' in out, (code, out, err, errors)
    code, out, err, errors = execute(netcell, ['python', '-c', "import urllib.request,urllib.error\ntry: urllib.request.urlopen('http://example.com',timeout=3)\nexcept urllib.error.HTTPError as e: assert e.code==403; print('denied')\nelse: raise RuntimeError('bypass')"])
    assert code == 0 and b'denied' in out, (code,out,err,errors)
    bypass = f"import socket\ns=socket.socket();s.settimeout(1)\ntry: s.connect(('{host_ip}',18081))\nexcept OSError: print('direct-blocked')\nelse: raise RuntimeError('direct bypass')"
    assert b'direct-blocked' in execute(netcell, ['python', '-c', bypass])[1]
    print('PASS approved inference route, denied proxy destination, direct connection blocked')

    source = (ROOT/'examples/contained-agent.py').read_text()
    agent = create(mode='agent', command=['python', '-u', '-c', source, f'http://{host_ip}:18081/v1', 'fixture'], allow=[{'host': host_ip, 'port': 18081, 'allow_private': True}])
    result = finished(agent)
    assert result['exit_code'] == 0 and result['state'] == 'exited', result
    ev = events(agent)
    assert any(e['event_kind'] == 'process.exec' for e in ev), ev
    assert any('model-tool-ok' in json.dumps(e) for e in ev)
    assert any(e['event_kind'] == 'network.allow' for e in ev)
    print('PASS contained agent → inference fixture → sandboxed tool; real SecCheck audit events')

    timed = create(timeout_seconds=2)
    assert finished(timed)['state'] == 'failed'
    limited = create(mode='agent', memory_mib=64, workspace_mib=16, command=['python','-c','x=bytearray(512*1024*1024)'])
    assert finished(limited)['exit_code'] != 0
    print('PASS wall-time and memory limits')

    process_limited = create(pids=16, memory_mib=256)
    fork_test = """import os,signal,errno
children=[]
try:
    for _ in range(40):
        try: pid=os.fork()
        except OSError as error:
            assert error.errno==errno.EAGAIN, error
            print('process-limit-enforced',flush=True)
            break
        if pid==0:
            signal.pause()
            os._exit(0)
        children.append(pid)
    else: raise AssertionError('process limit was not enforced')
finally:
    for pid in children:
        os.kill(pid,signal.SIGKILL)
        os.waitpid(pid,0)
"""
    code, out, err, errors = execute(process_limited, ['python', '-u', '-c', fork_test])
    assert code == 0 and b'process-limit-enforced' in out, (code,out,err,errors)
    print('PASS guest process-count enforcement')

    api('DELETE', '/v1alpha1/cells/' + c['id'])
    assert events(c), 'audit history disappeared after deletion'
    compose = ['docker','compose','-f',str(ROOT/'deploy/observability/compose.yaml')]
    subprocess.run(compose+['stop','loki'],check=True,stdout=subprocess.DEVNULL)
    try:
        assert execute(netcell, ['echo', 'loki-outage-recovery-marker'])[0] == 0
        assert any('loki-outage-recovery-marker' in json.dumps(e) for e in events(netcell))
    finally:
        subprocess.run(compose+['start','loki'],check=True,stdout=subprocess.DEVNULL)
    query = '{service_name="relayna-cell"} |= "' + netcell['id'] + '" |= "loki-outage-recovery-marker"'
    for attempt in range(45):
        try:
            found = query_loki(query)
            if found:
                break
        except Exception:
            pass
        time.sleep(1)
    else:
        raise AssertionError('logs did not reach Loki after recovery')
    series_url = 'http://127.0.0.1:3100/loki/api/v1/series?' + urllib.parse.urlencode({'match[]': '{service_name="relayna-cell"}'})
    with urllib.request.urlopen(series_url, timeout=10) as response:
        series = json.load(response)['data']
    for labels in series:
        assert 'cell_id' not in labels, labels
        assert 'filename' not in labels, labels
    print('PASS Loki outage recovery, retained audit history, bounded label cardinality')
    print('ALL INTEGRATION CHECKS PASSED')
finally:
    fixture.shutdown()
    for cell_id in cells:
        try:
            api('DELETE', '/v1alpha1/cells/' + cell_id)
        except Exception as exc:
            print('cleanup:', cell_id, str(exc))
