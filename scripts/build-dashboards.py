#!/usr/bin/env python3
"""Generate the provisioned Grafana dashboards from the existing audit schema."""
import argparse
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DEST = ROOT / 'deploy/observability/grafana/dashboards'
DS = {'type': 'loki', 'uid': 'cell-loki'}
COLORS = {'control': 'blue', 'runtime': 'purple', 'network': 'orange', 'workload': '#008a8a',
          'exited': 'green', 'failed': 'red', 'deleted': 'blue', 'interrupted': 'orange',
          'Exit 0': 'green', 'Non-zero exit': 'orange', 'Execution error': 'red',
          'Allowed': 'green', 'Denied': 'red', 'Error': 'orange', 'session': 'blue', 'agent': 'purple'}


def stream(source=None):
    labels = 'service_name="relayna-cell"' + (f',source="{source}"' if source else '')
    return '{'+labels+'} | cell_id=~"${cell_id:raw}"'


def kind(source, name):
    return stream(source) + f' | event_kind="{name}"'


def unique(query, key='event_id', group=None):
    """Deduplicate stable event IDs for whole-window totals (replay-safe)."""
    labels = key + (','+group if group else '')
    outer = 'count' + (f' by ({group})' if group else '')
    return f'{outer}(sum by ({labels}) (count_over_time({query} | __error__="" [$__range])))'


def count(query):
    return unique(query) + ' or vector(0)'


def bucket(query, group=None):
    keep = group or 'source'
    return 'sum'+(f' by ({group})' if group else '')+f'(count_over_time({query} | __error__="" | keep {keep} [$__interval]))'


def target(expr, legend='', instant=False, ref='A', lines=None):
    t = {'refId': ref, 'expr': expr, 'queryType': 'instant' if instant else 'range',
         'legendFormat': legend, 'datasource': DS, 'editorMode': 'code'}
    if lines:
        t['maxLines'] = lines
        t['direction'] = 'backward'
    return t


def overrides(names):
    return [{'matcher': {'id': 'byName', 'options': name},
             'properties': [{'id': 'color', 'value': {'mode': 'fixed', 'fixedColor': COLORS[name]}}]}
            for name in names]


def panel(pid, title, ptype, rect, queries, description=''):
    x, y, w, h = rect
    return {'id': pid, 'title': title, 'type': ptype, 'gridPos': {'x': x, 'y': y, 'w': w, 'h': h},
            'datasource': DS, 'description': description, 'targets': queries,
            'fieldConfig': {'defaults': {'unit': 'short', 'decimals': 0, 'min': 0,
                                         'color': {'mode': 'palette-classic'}, 'noValue': '—'}, 'overrides': []},
            'options': {}}


def stat(pid, title, x, query, color, description, unit='short', width=4, alarm=False):
    p = panel(pid, title, 'stat', (x, 0, width, 4), [target(query, title, True)], description)
    d = p['fieldConfig']['defaults']
    d.update(unit=unit, color={'mode': 'fixed', 'fixedColor': color})
    if unit == 'percent':
        d.update(decimals=1, max=100)
    if alarm:
        d.update(color={'mode': 'thresholds'}, thresholds={'mode': 'absolute', 'steps': [
            {'color': 'green', 'value': None}, {'color': 'red', 'value': 1}]})
    p['options'] = {'reduceOptions': {'calcs': ['lastNotNull'], 'fields': '', 'values': False},
                    'orientation': 'auto', 'textMode': 'value', 'colorMode': 'value',
                    'graphMode': 'none', 'justifyMode': 'center', 'wideLayout': True}
    return p


def timeseries(pid, title, rect, queries, colors, description, bars=False):
    p = panel(pid, title, 'timeseries', rect, queries, description)
    p.update(interval='1m', maxDataPoints=120)
    p['fieldConfig']['defaults']['custom'] = {
        'drawStyle': 'bars' if bars else 'line', 'lineInterpolation': 'linear',
        'lineWidth': 1 if bars else 2, 'fillOpacity': 65 if bars else 12,
        'showPoints': 'never', 'spanNulls': False,
        'stacking': {'mode': 'normal', 'group': 'A'},
        'axisPlacement': 'left', 'axisLabel': 'Events', 'axisCenteredZero': False,
        'scaleDistribution': {'type': 'linear'}, 'hideFrom': {'legend': False, 'tooltip': False, 'viz': False}}
    p['fieldConfig']['overrides'] = overrides(colors)
    p['options'] = {'legend': {'displayMode': 'list', 'placement': 'bottom', 'showLegend': True},
                    'tooltip': {'mode': 'multi', 'sort': 'desc'}}
    return p


def pie(pid, title, rect, expr, legend, colors, description):
    p = panel(pid, title, 'piechart', rect, [target(expr, legend, True)], description)
    # Loki instant vectors arrive as table rows in Grafana. Turn each category
    # into a named numeric field before reducing, or only the last row is shown.
    p['transformations'] = category_fields(legend)
    p['fieldConfig']['overrides'] = overrides(colors)
    p['options'] = {'pieType': 'donut', 'displayLabels': ['percent'],
                    'legend': {'displayMode': 'table', 'placement': 'bottom', 'showLegend': True, 'values': ['value']},
                    'tooltip': {'mode': 'single', 'sort': 'desc'},
                    'reduceOptions': {'values': False, 'calcs': ['lastNotNull'], 'fields': ''}}
    return p


def bars(pid, title, rect, expr, legend, description, color='blue'):
    p = panel(pid, title, 'bargauge', rect, [target(expr, legend, True)], description)
    p['transformations'] = category_fields(legend)
    p['transformations'].insert(1, {'id': 'sortBy', 'options': {
        'sort': [{'field': 'Value #A', 'desc': True}]}})
    p['fieldConfig']['defaults']['color'] = {'mode': 'fixed', 'fixedColor': color}
    p['options'] = {'orientation': 'horizontal', 'displayMode': 'gradient', 'valueMode': 'color',
                    'namePlacement': 'left', 'showUnfilled': True, 'minVizHeight': 16, 'minVizWidth': 8,
                    'reduceOptions': {'values': False, 'calcs': ['lastNotNull'], 'fields': ''},
                    'sizing': 'auto', 'text': {'titleSize': 12, 'valueSize': 16}}
    return p


def category_fields(legend):
    return [{'id': 'organize', 'options': {'excludeByName': {'Time': True}}},
            {'id': 'rowsToFields', 'options': {'mappings': [
        {'fieldName': legend.strip('{}'), 'handlerKey': 'field.name'},
        {'fieldName': 'Value #A', 'handlerKey': 'field.value'},
    ]}}]


def logs(pid, title, rect, query, description, limit=100, trusted=True):
    if trusted:
        # Loki's text heuristic mistakes an empty error key or command argument
        # for an error. Derive display severity only from trusted envelope data.
        severity = '{{if or .audit_error (eq .event_kind "audit.gap") (eq .event_kind "network.error")}}error{{else if eq .event_kind "network.deny"}}warn{{else}}info{{end}}'
        stage = ' | json audit_error="data.error" | label_format detected_level=`'+severity+'` | label_format level="{{.detected_level}}"'
        query = query.replace(' | line_format ', stage+' | line_format ', 1) if ' | line_format ' in query else query+stage
    p = panel(pid, title, 'logs', rect, [target(query, lines=limit)], description)
    p['options'] = {'showTime': True, 'showLabels': False, 'showCommonLabels': False,
                    'sortOrder': 'Descending', 'wrapLogMessage': True, 'prettifyLogMessage': False,
                    'enableLogDetails': True, 'dedupStrategy': 'exact'}
    return p


def dashboard(uid, title, description):
    return {'uid': uid, 'title': title, 'description': description, 'schemaVersion': 41,
            'version': 2, 'editable': False, 'tags': ['relayna-cell'], 'refresh': '15s',
            'timezone': 'browser', 'time': {'from': 'now-1h', 'to': 'now'},
            'timepicker': {'refresh_intervals': ['5s', '15s', '30s', '1m', '5m']},
            'links': [{'title': name, 'type': 'link', 'url': url, 'includeVars': True,
                       'keepTime': True, 'targetBlank': False, 'icon': 'dashboard'}
                      for name, url in [('Overview', '/d/relayna-cell/relayna-cell'),
                                        ('Investigate a Cell', '/d/relayna-cell-audit/relayna-cell-investigation')]],
            'templating': {'list': [{'name': 'cell_id', 'type': 'textbox', 'label': 'Cell ID (regex)',
                                    'current': {'text': '.*', 'value': '.*'}, 'query': '.*'}]},
            'panels': []}


requested = kind('control', 'cell.requested')
completed = stream('control') + ' | event_kind=~"execution.finished|execution.failed"'
finished = kind('control', 'execution.finished') + ' | json code="data.exit_code"'
success = finished + ' | code="0"'
nonzero = finished + ' | code!="0"'
execs = kind('runtime', 'process.exec')
denied = kind('network', 'network.deny')
gaps = kind('control', 'audit.gap')
cell_end = kind('control', 'cell.finished') + ' | json state="data.state", code="data.exit_code", error="data.error"'
default_scope = 'For the selected time range and Cell filter. Counts reflect events received by Loki, not current live state.'
window_note = 'Events in each displayed interval; the interval adapts to the time range. Missing buckets indicate no matching records, not a service-health guarantee.'

overview = dashboard('relayna-cell', 'Relayna Cell', 'Activity, execution outcomes and enforcement from the Loki audit projection. Integration-test traffic and expected restriction failures are included. CPU/memory utilization and live-state gauges require separate telemetry.')
overview['panels'] = [
    stat(1, 'Cells requested', 0, unique(requested, 'cell_id')+' or vector(0)', 'blue', default_scope+' Distinct Cell IDs with a cell.requested record.'),
    stat(2, 'API executions completed', 4, count(completed), '#008a8a', default_scope+' Includes command and file-transfer helper executions that finished or failed.'),
    stat(3, 'API execution success', 8, '100 * ('+count(success)+') / ('+count(completed)+')', 'green', 'Exit-code-zero executions / all finished or failed API executions. Includes workspace helpers. No value when there are no completed executions.', 'percent'),
    stat(4, 'Process exec events', 12, count(execs), 'purple', default_scope+' Successful exec transitions observed by SecCheck; initial container starts and clone events are separate.'),
    stat(5, 'Network denials', 16, count(denied), 'orange', default_scope+' Proxy-denied destinations. Intentional restriction tests are included.'),
    stat(6, 'Audit gap reports', 20, count(gaps), 'green', default_scope+' Zero means no gap reports were received; it does not prove complete coverage.', alarm=True),
    timeseries(10, 'Audit activity over time', (0, 4, 16, 8), [target(bucket(stream(), 'source'), '{{source}}')],
               ['control', 'runtime', 'network', 'workload'], window_note+' Runtime activity includes process clones and exits.', True),
    pie(11, 'Cell outcomes', (16, 4, 8, 8), unique(cell_end, group='state'), '{{state}}',
        ['exited', 'failed', 'deleted', 'interrupted'], 'State recorded in cell.finished, deduplicated by event ID. Deleted means the Cell was stopped by deletion. Exited describes lifecycle, not necessarily a zero process exit code. Preparation failures have no finished event.'),
    timeseries(12, 'API execution outcomes', (0, 12, 12, 8), [
        target(bucket(success), 'Exit 0'), target(bucket(nonzero), 'Non-zero exit', ref='B'),
        target(bucket(kind('control', 'execution.failed')), 'Execution error', ref='C')],
        ['Exit 0', 'Non-zero exit', 'Execution error'], window_note+' Non-zero exits can be expected restriction-test results.', True),
    timeseries(13, 'Network decisions over time', (12, 12, 12, 8), [
        target(bucket(kind('network', 'network.allow')), 'Allowed'),
        target(bucket(denied), 'Denied', ref='B'), target(bucket(kind('network', 'network.error')), 'Error', ref='C')],
        ['Allowed', 'Denied', 'Error'], window_note+' Allowed is an authorization decision, not proof of a completed upstream connection.', True),
    bars(14, 'Most executed binaries', (0, 20, 9, 7),
         'topk(6, '+unique(execs+' | json binary="data.binary_path" | binary!=""', group='binary')+')', '{{binary}}',
         'Successful process.exec events, deduplicated by event ID. Shows up to six binary paths.', 'purple'),
    bars(15, 'Most requested destinations', (9, 20, 8, 7),
         'topk(6, '+unique(stream('network')+' | event_kind=~"network.allow|network.deny|network.error" | json destination="data.destination"', group='destination')+')', '{{destination}}',
         'All proxy decisions grouped by destination, including allowed, denied and lookup-error records.', 'orange'),
    pie(16, 'Workload modes', (17, 20, 7, 7), unique(requested+' | json mode="data.mode"', 'cell_id', 'mode'), '{{mode}}',
        ['session', 'agent'], 'Distinct requested Cells by mode. Session: delegated commands. Agent: the entire workload runs inside Cell.'),
]

table = panel(20, 'Recent Cell outcomes · click a Cell to investigate', 'table', (0, 27, 24, 9),
              [target(cell_end+' | line_format `{"Cell":"{{.cell_id}}","State":"{{.state}}","Exit code":{{if eq .state "exited"}}{{.code}}{{else}}null{{end}}}`', lines=100)],
              'Latest 100 cell.finished records. Exit code is shown only for the exited state; deletion and timeout defaults are not process success. Outcome is at finish time; later deletion does not rewrite this historical event. Open the Cell link for process and command evidence.')
table['transformations'] = [
    {'id': 'extractFields', 'options': {'source': 'Line', 'format': 'json', 'replace': True, 'keepTime': True}},
    {'id': 'organize', 'options': {'indexByName': {'Time': 0, 'Cell': 1, 'State': 2, 'Exit code': 3}}},
]
table['fieldConfig']['defaults'].update(custom={'align': 'auto', 'cellOptions': {'type': 'auto'}, 'inspect': False}, min=None)
table['fieldConfig']['overrides'] = [
    {'matcher': {'id': 'byName', 'options': 'Time'}, 'properties': [{'id': 'unit', 'value': 'dateTimeAsLocal'}, {'id': 'custom.width', 'value': 185}]},
    {'matcher': {'id': 'byName', 'options': 'Cell'}, 'properties': [{'id': 'custom.width', 'value': 335}, {'id': 'links', 'value': [
        {'title': 'Investigate this Cell', 'url': '/d/relayna-cell-audit/relayna-cell-investigation?var-cell_id=${__value.raw}&from=${__from}&to=${__to}', 'targetBlank': False}]}]},
    {'matcher': {'id': 'byName', 'options': 'State'}, 'properties': [
        {'id': 'custom.cellOptions', 'value': {'type': 'color-text'}},
        {'id': 'mappings', 'value': [{'type': 'value', 'options': {s: {'text': s.capitalize(), 'color': COLORS[s]} for s in ['exited','failed','deleted','interrupted']}}]}]},
]
table['options'] = {'showHeader': True, 'cellHeight': 'sm', 'footer': {'show': False},
                    'sortBy': [{'displayName': 'Time', 'desc': True}]}
overview['panels'].append(table)
attention = stream()+' | event_kind=~"network.deny|audit.gap|execution.failed|cell.cleanup_failed" | json destination="data.destination", reason="data.reason", error="data.error" | line_format `{{.event_kind}}  cell={{.cell_id}}  {{.destination}} {{.reason}}{{.error}}`'
output = stream('workload')+' | json text="data.text", channel="data.stream" | line_format `{{.channel}}  {{.text}}`'
overview['panels'] += [
    logs(21, 'Denials and audit exceptions', (0, 36, 12, 9), attention, 'Latest 100 denials, execution errors, cleanup failures and audit-gap reports. Expand a line for its original fields.'),
    logs(22, 'Recent workload output', (12, 36, 12, 9), output, 'Workload-provided text, not trusted evidence of policy enforcement. Includes contained SDK messages. Expand a line to see its Cell and execution IDs.', trusted=False),
]

# Keep full metric names readable with Grafana's navigation drawer open.
for p in overview['panels']:
    if p['type'] == 'stat':
        index = p['id'] - 1
        p['gridPos'].update(x=(index % 3)*8, y=(index // 3)*3, w=8, h=3)
        p['options']['text'] = {'valueSize': 40}
    else:
        p['gridPos']['y'] += 2

audit = dashboard('relayna-cell-audit', 'Relayna Cell · Investigation', 'Inspect a Cell using the shared Cell ID and time filters. Readable logs retain expandable original fields; raw JSON is available in the collapsed final section.')
audit['panels'] = [
    stat(1, 'API executions', 0, count(completed), '#008a8a', default_scope+' Finished or failed API executions, including workspace helpers.', width=6),
    stat(2, 'Process exec events', 6, count(execs), 'purple', default_scope, width=6),
    stat(3, 'Network denials', 12, count(denied), 'orange', default_scope, width=6),
    stat(4, 'Workload log records', 18, count(kind('workload', 'output')), 'blue', 'Journal output chunks, not lines, tool calls or tokens.', width=6),
    timeseries(10, 'Cell activity', (0, 4, 16, 7), [target(bucket(stream(), 'source'), '{{source}}')],
               ['control', 'runtime', 'network', 'workload'], window_note, True),
    pie(11, 'Evidence sources', (16, 4, 8, 7), 'sum by (source)(count_over_time('+stream()+' | keep source [$__range]))', '{{source}}',
        ['control', 'runtime', 'network', 'workload'], 'Received record counts by producer. Workload content is self-reported; control, network and runtime are separate trusted producers.'),
    logs(20, 'Lifecycle', (0, 11, 12, 10), stream('control')+' | event_kind=~"cell.*|audit.gap" | json state="data.state", mode="data.mode", image="data.image", error="data.error" | line_format `{{.event_kind}}  {{if .state}}state={{.state}} {{end}}{{if .mode}}mode={{.mode}} {{end}}{{.image}} {{.error}}`', 'Cell admission, preparation, completion, deletion and audit gaps. Select a Cell ID to isolate one lifecycle.'),
    logs(21, 'API commands and results', (12, 11, 12, 10), stream('control')+' | event_kind=~"execution.*" | json command="data.command", code="data.exit_code", error="data.error" | line_format `{{.event_kind}}  exec={{.execution_id}}  {{.command}}{{if .code}} exit={{.code}}{{end}} {{.error}}`', 'Commands submitted through the API and their results. Includes workspace file helpers. Contained-agent child commands appear in Observed processes.'),
    logs(22, 'Observed processes', (0, 21, 12, 11), stream('runtime')+' | event_kind=~"container.start|process.exec|process.exit" | json pid="data.context_data.thread_group_id", binary="data.binary_path", argv="data.argv", args="data.args", status="data.exit_status" | line_format `{{.event_kind}}  pid={{.pid}}  {{.binary}} {{.argv}}{{.args}}{{if .status}} status={{.status}}{{end}}`', 'Runtime observation of starts, exec transitions and exits. Clone noise is hidden here; the raw event view includes it. Exit status is the runtime wait status, not the API exit code.'),
    logs(23, 'Network decisions', (12, 21, 12, 11), stream('network')+' | json destination="data.destination", method="data.method", ip="data.ip", reason="data.reason" | line_format `{{.event_kind}}  {{.method}} {{.destination}}  {{if .ip}}ip={{.ip}}{{end}} {{.reason}}`', 'Proxy authorization decisions for the selected Cells. Direct socket observations remain in raw runtime events.'),
    logs(24, 'Workload output · stdout and stderr', (0, 32, 24, 11), output, 'Untrusted workload output. Contained OpenAI Agents SDK events are visible here. Expand a line to correlate Cell and execution IDs.', 300, trusted=False),
    {'id': 30, 'type': 'row', 'title': 'Raw audit events · full JSON', 'collapsed': True,
     'gridPos': {'x': 0, 'y': 43, 'w': 24, 'h': 1}, 'panels': [
         logs(31, 'Raw audit events', (0, 44, 24, 14), stream(), 'Original event envelopes, including process clones and syscall records. Latest 500 records in the selected range.', 500, trusted=False)]},
]

DEST.mkdir(parents=True, exist_ok=True)
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--check', action='store_true', help='Fail if the committed JSON needs regeneration')
args = parser.parse_args()
for name, doc in [('cell.json', overview), ('cell-audit.json', audit)]:
    rendered = json.dumps(doc, indent=2)+'\n'
    if args.check:
        if not (DEST/name).exists() or (DEST/name).read_text() != rendered:
            raise SystemExit(f'{name} is stale; run python3 scripts/build-dashboards.py')
        print(f'Checked {name}')
    else:
        (DEST/name).write_text(rendered)
        print(f'Wrote {name}: {len(doc["panels"])} panels')
