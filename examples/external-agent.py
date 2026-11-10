"""An external Python application delegates tools through the Mac CLI."""
import json
import pathlib
import subprocess

root = pathlib.Path(__file__).resolve().parents[1]
cli = [str(root/'bin/cell'), '--lima', 'relayna-cell']
created = subprocess.check_output(cli + ['create', '-f', str(root/'examples/session.json')])
cell_id = json.loads(created)['id']
try:
    subprocess.run(cli + ['exec', cell_id, '--', 'sh', '-c', 'echo external-agent-result > /workspace/result.txt'], check=True)
    result = subprocess.check_output(cli + ['get', cell_id, 'result.txt'])
    print(result.decode(), end='')
finally:
    subprocess.run(cli + ['delete', cell_id], check=True)
