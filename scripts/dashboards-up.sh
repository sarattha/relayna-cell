#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
python3 scripts/build-dashboards.py
COPYFILE_DISABLE=1 tar --no-xattrs -cf - deploy/observability/grafana/dashboards | limactl shell relayna-cell sudo tar -xf - -C /opt/relayna-cell
# Grafana's file provider polls the mounted directory; no service restart needed.
python3 - <<'PY'
import json,pathlib,time,urllib.request
expected={}
for path in pathlib.Path('deploy/observability/grafana/dashboards').glob('*.json'):
    doc=json.loads(path.read_text())
    expected[doc['uid']]=doc
def signature(doc):
    return doc['panels']
for attempt in range(30):
    ready=True
    for uid, wanted in expected.items():
        try:
            with urllib.request.urlopen('http://localhost:3300/api/dashboards/uid/'+uid, timeout=3) as r:
                doc=json.load(r)
            ready &= doc['meta']['provisioned'] and signature(doc['dashboard'])==signature(wanted)
        except Exception:
            ready=False
    if ready:
        print('Grafana overview and investigation dashboards are provisioned')
        break
    time.sleep(1)
else:
    raise SystemExit('Dashboard provisioning did not complete; inspect Grafana logs')
PY
