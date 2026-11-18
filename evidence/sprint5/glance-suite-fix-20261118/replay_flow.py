"""Replay a past flow window on the VM's persistent logs with two versions of glance_flow."""
import importlib.util, json, subprocess, sys

def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

old, new = load('flow_old', sys.argv[1]), load('flow_new', sys.argv[2])
unit, offsets, window_s, size = sys.argv[3], json.load(open(sys.argv[4])), float(sys.argv[5]), int(sys.argv[6])
start, end = int(sys.argv[7]), int(sys.argv[8])  # run bounds, µs
every = new.go_events(new.journal([unit], start, end))
ts = [e['ts_us'] for e in every]
first = next(i for i in range(len(ts) - len(offsets) + 1)
             if all(abs((ts[i + k] - ts[i]) / 1000 - off) < 1.0 for k, off in enumerate(offsets)))
since = ts[first] - 300_000
assert first == 0 or ts[first - 1] < since, 'suite request inside the replayed window'
until = since + int(window_s * 1_000_000)
apache_lines = subprocess.run(['sudo', '-n', 'cat', '/var/log/apache2/horizon_access.log'], check=True,
                              capture_output=True, text=True).stdout.splitlines()
for label, flow in (('viejo', old), ('nuevo', new)):
    go = flow.within(flow.go_events(flow.journal([unit], since, until)), since, until)
    calls, requests = (flow.within(e, since, until) for e in flow.nova_events(flow.journal(flow.NOVA_UNITS, since, until)))
    timeline = flow.correlate(go, calls, requests, flow.apache_events(apache_lines, since, until))
    summary = flow.summarize(timeline, size)
    print(label, 'aceptado' if flow.flow_accepted(summary) else 'RECHAZADO', [e['caller'] for e in timeline],
          {k: summary[k] for k in ('metadata_reads_200', 'downloads_200_full', 'non_nova_glance_requests')})
