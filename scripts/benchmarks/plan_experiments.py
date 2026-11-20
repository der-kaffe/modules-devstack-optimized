#!/usr/bin/env python3
"""Generate immutable draft schedules without running or switching any services."""
import argparse
import hashlib
import json
from pathlib import Path
import random
import uuid


def schedule(protocol, kind, campaign, pilot):
    rng = random.Random(protocol['seed'])
    campaign = str(uuid.UUID(campaign))
    plan = []
    if kind == 'glance':
        for scenario in protocol['metadata_scenarios'] + protocol['binary_scenarios']:
            binary = scenario in protocol['binary_scenarios']
            concurrencies = protocol['binary_concurrency_candidates'] if binary else protocol['metadata_concurrency_candidates']
            for concurrency in concurrencies:
                for size in protocol['payload_bytes_candidates'] if binary else [None]:
                    first = rng.choice(['python', 'go'])
                    for repetition in range(1, (1 if pilot else protocol['repetitions']) + 1):
                        order = [first, 'go' if first == 'python' else 'python']
                        if repetition % 2 == 0:
                            order.reverse()
                        key = f'{scenario}-c{concurrency}-b{size}-r{repetition}'
                        plan.append({'pair': key, 'repetition': repetition, 'scenario': scenario,
                                     'concurrency': concurrency, 'payload_bytes': size,
                                     'campaign_id': str(uuid.uuid5(uuid.UUID(campaign), key)),
                                     'order': order})
    elif kind == 'integrated':
        # Balanced cyclic orders; no assumed additivity of service effects.
        base = list('ABCD')
        rng.shuffle(base)
        for repetition in range(1, (1 if pilot else protocol['repetitions']) + 1):
            n = (repetition - 1) % 4
            order = base[n:] + base[:n]
            plan.append({'repetition': repetition, 'order': order,
                         'campaign_id': str(uuid.uuid5(uuid.UUID(campaign), str(repetition)))})
    else:
        raise ValueError('unknown experiment')
    return {'schema': 1, 'campaign_id': campaign, 'kind': kind,
            'stage': 'pilot-plan' if pilot else 'reference-plan-not-executable',
            'protocol': protocol['protocol'], 'protocol_status': protocol['status'],
            'execution_authorized_by_plan': False, 'official_accepted': False,
            'seed': protocol['seed'], 'blocks': plan}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--kind', choices=('glance', 'integrated'), required=True)
    p.add_argument('--protocol', type=Path, required=True)
    p.add_argument('--campaign-id', default=str(uuid.uuid4()))
    p.add_argument('--pilot', action='store_true')
    p.add_argument('--output', type=Path, required=True)
    args = p.parse_args()
    raw = args.protocol.read_bytes()
    result = schedule(json.loads(raw), args.kind, args.campaign_id, args.pilot)
    result['protocol_sha256'] = hashlib.sha256(raw).hexdigest()
    with args.output.open('x') as stream:
        json.dump(result, stream, indent=2)
        stream.write('\n')


if __name__ == '__main__':
    main()
