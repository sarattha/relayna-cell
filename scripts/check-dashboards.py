#!/usr/bin/env python3
"""Check live Loki panel queries and reconcile totals against raw event envelopes.

Requires the local Grafana/Loki stack; writes only a small verification report.
Run after a smoke test to exercise non-empty categories as well as empty states.
"""
import collections
import json
import math
from pathlib import Path
import time
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
BASE = 'http://localhost:3300'
LOKI = BASE + '/api/datasources/proxy/uid/cell-loki/loki/api/v1/'
END = time.time_ns()
WINDOW = 6 * 3600
START = END - WINDOW * 10**9


def get(url, params=None):
    if params:
        url += '?' + urllib.parse.urlencode(params)
    with urllib.request.urlopen(url, timeout=30) as response:
        return json.load(response)


def query(expr, instant=False, limit=5000):
    params = {'query': expr, 'limit': limit}
    if instant:
        params['time'] = END
    else:
        params.update(start=START, end=END, step=60, direction='backward')
    doc = get(LOKI + ('query' if instant else 'query_range'), params)
    assert doc['status'] == 'success', doc
    assert not any(r.get('stream', {}).get('__error__') for r in doc['data']['result']), f'Log parsing failed: {expr}'
    return doc['data']['result']


def scalar(result):
    return float(result[0]['value'][1]) if result else 0


def categories(result, label):
    return {r['metric'][label]: int(float(r['value'][1])) for r in result}


def close(actual, expected):
    return (math.isnan(actual) and math.isnan(expected)) or math.isclose(actual, expected)


def all_panels(panels):
    for panel in panels:
        yield panel
        yield from all_panels(panel.get('panels', []))


records = query('{service_name="relayna-cell"}')
lines = [line for stream in records for _, line, *_ in stream['values']]
assert len(lines) < 5000, 'Raw verification window reached the limit; narrow it before comparing totals'
events = {event['event_id']: event for event in map(json.loads, lines)}
assert events, 'Run make smoke or make model-smoke first to provide real audit events'
sample = next((e['cell_id'] for e in reversed(list(events.values())) if e['source'] == 'network'),
              next(iter(events.values()))['cell_id'])
docs = [json.loads(p.read_text()) for p in sorted((ROOT/'deploy/observability/grafana/dashboards').glob('*.json'))]
report = {'start_ns': START, 'end_ns': END, 'raw_records': len(lines), 'unique_events': len(events), 'scopes': []}

for cell_filter in ['.*', sample, 'no-such-cell']:
    selected = [e for e in events.values() if cell_filter == '.*' or e.get('cell_id') == cell_filter]
    kinds = collections.Counter(e['event_kind'] for e in selected)
    completed = kinds['execution.finished'] + kinds['execution.failed']
    ok = sum(e['event_kind'] == 'execution.finished' and e['data'].get('exit_code') == 0 for e in selected)
    expected = {
        1: len({e['cell_id'] for e in selected if e['event_kind'] == 'cell.requested'}),
        2: completed, 3: 100 * ok / completed if completed else float('nan'),
        4: kinds['process.exec'], 5: kinds['network.deny'], 6: kinds['audit.gap'],
    }
    expected_groups = {
        11: ('state', collections.Counter(e['data']['state'] for e in selected if e['event_kind'] == 'cell.finished')),
        14: ('binary', collections.Counter(e['data']['binary_path'] for e in selected if e['event_kind'] == 'process.exec' and e['data'].get('binary_path'))),
        15: ('destination', collections.Counter(e['data']['destination'] for e in selected if e['event_kind'] in ['network.allow', 'network.deny', 'network.error'])),
        16: ('mode', collections.Counter(mode for _, mode in {(e['cell_id'], e['data']['mode']) for e in selected if e['event_kind'] == 'cell.requested'})),
    }
    checked = 0
    for doc in docs:
        deployed = get(BASE+'/api/dashboards/uid/'+doc['uid'])
        assert deployed['meta']['provisioned'], doc['uid']
        assert deployed['dashboard']['panels'] == doc['panels'], f'{doc["uid"]} is not current; run make dashboards-up'
        for panel in all_panels(doc['panels']):
            for target in panel.get('targets', []):
                expr = (target['expr'].replace('${cell_id:raw}', cell_filter)
                        .replace('$__range', f'{WINDOW}s').replace('$__interval', '60s'))
                result = query(expr, target['queryType'] == 'instant', target.get('maxLines', 5000))
                checked += 1
                if doc['uid'] != 'relayna-cell':
                    continue
                if panel['id'] in expected:
                    actual = scalar(result)
                    assert close(actual, expected[panel['id']]), (cell_filter, panel['title'], actual, expected[panel['id']])
                if panel['id'] in expected_groups:
                    label, counts = expected_groups[panel['id']]
                    actual = categories(result, label)
                    if panel['id'] in (14, 15) and len(counts) > 6:
                        assert len(actual) == 6 and all(v == counts[k] for k, v in actual.items())
                        assert min(actual.values()) >= sorted(counts.values(), reverse=True)[5]
                    else:
                        assert actual == dict(counts), (cell_filter, panel['title'], actual, dict(counts))
    report['scopes'].append({'cell_filter': cell_filter, 'queries_checked': checked,
                             'totals': {str(k): (v if math.isfinite(v) else None) for k, v in expected.items()}})
    print(f'{cell_filter}: {checked} queries passed; totals and category counts match raw Loki records')

path = ROOT/'.state/dashboard/verification.json'
path.parent.mkdir(parents=True, exist_ok=True)
path.write_text(json.dumps(report, indent=2)+'\n')
print(f'Wrote {path}')
