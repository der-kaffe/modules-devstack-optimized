#!/usr/bin/env python3
"""Publication plots of complete paired blocks; no fabricated observations."""
import argparse
import hashlib
import json
from pathlib import Path
import statistics
import sys

from experiment_analysis import glance_run


def plot(campaign_path,acceptance_path,output):
    accepted=json.loads(acceptance_path.read_text())
    campaign=json.loads(campaign_path.read_text())
    if not accepted.get('selected_block_accepted') or accepted['validated_windows']!=len(campaign['runs']):
        raise ValueError('accepted complete subset required')
    rows=[(r,glance_run(Path(r['path']))) for r in campaign['runs']]
    if any(not data['valid'] or not data['resources'] for _,data in rows):
        raise ValueError('raw verification rejected')
    import matplotlib
    matplotlib.use('Agg')
    import matplotlib.pyplot as plt
    output.mkdir(exist_ok=False)
    exports=[]
    metrics=[('Run p50 latency (ms)',lambda r:r['http']['correct_latency_ms']['p50']),
             ('Goodput (correct requests/s)',lambda r:r['http']['correct_ops_s']),
             ('API CPU per correct request (ms)',lambda r:r['resources']['cpu_s_per_correct_request']*1000),
             ('API mean PSS per run (MiB)',lambda r:r['resources']['pss_mean_bytes']/1024**2)]
    for case in accepted['selected_cases']:
        selected=[(meta,data) for meta,data in rows if [data['scenario'],data['concurrency'],data['payload']['bytes'] if data['payload'] else None]==case]
        reps=sorted({meta['repetition'] for meta,_ in selected})
        selected_metrics = list(metrics)
        if case[2]:
            selected_metrics += [('Useful transferred binary MiB/s',lambda r:sum(op['goodput_mib_s'] for op in r['operations'].values())),
                                 ('Run p99 latency (ms)',lambda r:r['http']['correct_latency_ms']['p99'])]
        nrows = (len(selected_metrics)+1)//2
        fig,axes=plt.subplots(nrows,2,figsize=(10,4*nrows))
        for axis,(label,extract) in zip(axes.flat,selected_metrics):
            observations={impl:[] for impl in ['python','go']}
            for index,rep in enumerate(reps):
                pair={m['implementation']:extract(r) for m,r in selected if m['repetition']==rep}
                if set(pair)!={'python','go'}:
                    raise ValueError('incomplete pair')
                jitter=(index-(len(reps)-1)/2)*.018
                axis.plot([jitter,1+jitter],[pair['python'],pair['go']],color='#b7b7b7',lw=1,zorder=1)
                for x,impl,color in [(0,'python','#446b9e'),(1,'go','#42836a')]:
                    observations[impl].append(pair[impl])
                    axis.scatter(x+jitter,pair[impl],color=color,s=35,zorder=3)
            for x,impl in enumerate(['python','go']):
                median=statistics.median(observations[impl])
                axis.plot([x-.16,x+.16],[median,median],color='black',lw=2,zorder=4)
            axis.set_xticks([0,1],['Python','Go'])
            axis.set_ylabel(label)
            axis.set_xlim(-.45,1.45)
            axis.set_ylim(bottom=0)
            axis.grid(axis='y',alpha=.2)
        size_label = f", payload {case[2]/1024**2:g} MiB" if case[2] else ""
        fig.suptitle(f"Glance {case[0]}, concurrency {case[1]}{size_label} — {len(reps)} paired repetitions\nProtocol SHA-256 {accepted['protocol_sha256'][:12]}",fontsize=12)
        fig.text(.05,.035,'Dots: individual runs; gray lines: pairs; black bars: medians. No pooled request-level inference.',fontsize=9)
        fig.text(.05,.016,'4 GiB guest / 4 vCPU; one Python worker / Go GOMAXPROCS=1; real Keystone; 30 s warm-up / 90 s measurement.',fontsize=8)
        fig.tight_layout(rect=(0,.065,1,.93))
        name=f'{case[0]}-c{case[1]}-b{case[2] or 0}-paired'
        for suffix in ('png','pdf'):
            fig.savefig(output/(name+'.'+suffix),dpi=200)
        plt.close(fig)
        exports.append({'case':case,'n_independent_pairs':len(reps),'figure':name})
    provenance={'stage':'official','selected_block_accepted':True,'full_protocol_complete':accepted['full_protocol_complete'],
                'campaign_path':str(campaign_path.resolve()),'campaign_sha256':hashlib.sha256(campaign_path.read_bytes()).hexdigest(),
                'acceptance_path':str(acceptance_path.resolve()),'acceptance_sha256':hashlib.sha256(acceptance_path.read_bytes()).hexdigest(),
                'figures':exports,'limitations':['API CPU and PSS exclude Keystone, SQL, Apache and generator; see separate component report',
                                               'single operation/concurrency subset; no whole-Glance or integrated conclusion']}
    (output/'provenance.json').write_text(json.dumps(provenance,indent=2)+'\n')
    (output/'SHA256SUMS').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest()+'  '+p.name+'\n' for p in sorted(output.iterdir()) if p.is_file()))


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--campaign',type=Path,required=True)
    p.add_argument('--acceptance',type=Path,required=True)
    p.add_argument('--output',type=Path,required=True)
    args=p.parse_args()
    plot(args.campaign,args.acceptance,args.output)


if __name__=='__main__':
    main()
