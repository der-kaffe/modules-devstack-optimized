#!/usr/bin/env python3
"""Keep API, Keystone, SQL, proxy and collector measurements separate."""
import argparse
import csv
import json
from pathlib import Path
import sys

from experiment_analysis import glance_run
from placement_resources import interval_summary


def analyze(root):
    verified=glance_run(root)
    if not verified['valid']:
        raise ValueError('invalid raw HTTP window')
    manifest=json.loads((root/'manifest.json').read_text())
    clock=json.loads((root/'clock-alignment.json').read_text())
    records=[json.loads(s) for s in (root/'resources.jsonl').read_text().splitlines()]
    requests=[json.loads(s) for s in (root/'samples.jsonl').read_text().splitlines()]
    begin=manifest['measurement_start_utc_ns']+clock['remote_minus_generator_ns']
    end=manifest['measurement_end_utc_ns']+clock['remote_minus_generator_ns']
    completions=[begin+s['finish_ns']-manifest['measurement_start_monotonic_ns'] for s in requests if s['phase']=='measurement' and s['valid']]
    groups={}
    for group in records[0]['groups']:
        try:
            groups[group]={'valid':True,'measurement':interval_summary(records,group,begin,end,completions,cores=clock['service_vcpus'])}
        except ValueError as error:
            groups[group]={'valid':False,'reason':str(error),'measurement':None}
    return {'run_id':manifest['run_id'],'stage':manifest['stage'],'implementation':manifest['implementation'],
            'groups':groups,'official_accepted':False,
            'limitation':'auxiliary processes may also serve unrelated lab activity; do not attribute their whole usage to Glance'}


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('runs',type=Path,nargs='+')
    p.add_argument('--output',type=Path,required=True)
    args=p.parse_args()
    try:
        rows=[analyze(root) for root in args.runs]
        args.output.mkdir(exist_ok=False)
        (args.output/'components.json').write_text(json.dumps(rows,indent=2)+'\n')
        with (args.output/'components.csv').open('x') as stream:
            writer=csv.writer(stream)
            writer.writerow(['run_id','stage','implementation','group','valid','cpu_seconds','covered_s','cpu_per_api_correct_request','pss_mean_bytes','pss_peak_bytes','rss_peak_bytes','reason'])
            for row in rows:
                for name,value in row['groups'].items():
                    m=value['measurement'] or {}
                    writer.writerow([row['run_id'],row['stage'],row['implementation'],name,value['valid'],
                        *[m.get(k) for k in ['cpu_seconds','covered_s','cpu_s_per_correct_request','pss_mean_bytes','pss_peak_observed_bytes','rss_peak_observed_bytes']],value.get('reason')])
        return 0
    except Exception as error:
        print('component analysis rejected: '+type(error).__name__,file=sys.stderr)
        return 2


if __name__=='__main__':
    sys.exit(main())
