#!/usr/bin/env python3
"""Plan an experiment fleet, or update its live policy without restarting nodes.

Only this operator command changes the budget. Spawn hooks read the policy;
they never increase it. Prices are planning inputs, not a billing stop switch.
"""
import argparse
from decimal import Decimal, ROUND_FLOOR, InvalidOperation
import json
import os
from pathlib import Path
import subprocess
import sys


def aws(region, *args):
    return json.loads(subprocess.check_output(
        ['aws', '--region', region, *args, '--output', 'json']))


def positive(value):
    number = int(value)
    if number < 1:
        raise ValueError('capacity values must be positive')
    return number


def bound(config, host_cpus, loader_cpus, quota, other_cpus, host_price, loader_price):
    """One machine ceiling; there is no second lifetime/vnode ceiling."""
    for key in ('hosts', 'loaders', 'ttl'):
        positive(config[key])
    dynamic = config.get('resource_slots') and config['slots'] == 0
    if not dynamic:
        positive(config['slots'])
    hours = Decimal(str(config['ttl'])) / 60
    budget = Decimal(str(config['budget']))
    host_price, loader_price = Decimal(str(host_price)), Decimal(str(loader_price))
    if (any(not v.is_finite() or v <= 0 for v in (budget, host_price, loader_price))
            or min(host_cpus, loader_cpus, quota) <= 0 or other_cpus < 0):
        raise ValueError('positive budget, prices and confirmed quota are required')
    limits = {
        'budget': int(((budget / hours - config['loaders'] * loader_price)
                       / host_price).to_integral_value(rounding=ROUND_FLOOR)),
        'quota': (quota - other_cpus - config['loaders'] * loader_cpus) // host_cpus,
    }
    if config.get('max_hosts') is not None:
        limits['operator'] = positive(config['max_hosts'])
    ceiling = min(limits.values())
    required = max(config["hosts"], config.get("min_peak_hosts") or 0)
    if ceiling < required:
        minimum_budget = hours * (required * host_price + config["loaders"] * loader_price)
        minimum_vcpus = other_cpus + required * host_cpus + config["loaders"] * loader_cpus
        raise ValueError(
            f"fleet requires {required} data hosts, ceiling={ceiling} ({limits}); "
            f"requires at least ${minimum_budget:.4f} of EC2 budget and "
            f"{minimum_vcpus} regional vCPUs at this TTL"
        )
    return dict(config, max_machines=ceiling, max_vnodes=None if dynamic else ceiling * config['slots'],
                limits=limits, reason='+'.join(k for k, v in limits.items() if v == ceiling),
                host_vcpus=host_cpus, loader_vcpus=loader_cpus, quota_vcpus=quota,
                other_vcpus=other_cpus, host_price=str(host_price), loader_price=str(loader_price))


def price(region, instance_type):
    filters = dict(regionCode=region, instanceType=instance_type, operatingSystem='Linux',
                   tenancy='Shared', preInstalledSw='NA', capacitystatus='Used')
    products = aws('us-east-1', 'pricing', 'get-products', '--service-code', 'AmazonEC2',
                   '--filters', *[f'Type=TERM_MATCH,Field={k},Value={v}' for k, v in filters.items()])
    rates = {d['pricePerUnit']['USD'] for raw in products['PriceList']
             for term in json.loads(raw)['terms'].get('OnDemand', {}).values()
             for d in term['priceDimensions'].values() if d['unit'] == 'Hrs'}
    if len(rates) != 1:
        raise ValueError(f'cannot resolve one Linux On-Demand price for {instance_type}; provide its price')
    return rates.pop()


def plan(config):
    region = config['region']
    if any(config[k][0] not in 'acdhimrtz' for k in ('host_type', 'loader_type')):
        raise ValueError('this planner requires standard-family On-Demand instance types')
    quota = int(aws(region, 'service-quotas', 'get-service-quota', '--service-code', 'ec2',
                    '--quota-code', 'L-1216C47A')['Quota']['Value'])
    instances = aws(region, 'ec2', 'describe-instances', '--filters',
                    'Name=instance-state-name,Values=pending,running')
    # Other On-Demand standard-family instances share this regional quota.
    other = [i for r in instances['Reservations'] for i in r['Instances']
             if i.get('InstanceLifecycle') != 'spot'
             and i['InstanceType'][0] in 'acdhimrtz'
             and not any(t['Key'] == 'Run' and t['Value'] == config['run'] for t in i.get('Tags', []))]
    kinds = sorted({config['host_type'], config['loader_type'], *[i['InstanceType'] for i in other]})
    cpus = {}
    for start in range(0, len(kinds), 100):
        data = aws(region, 'ec2', 'describe-instance-types', '--instance-types', *kinds[start:start + 100])
        cpus.update({t['InstanceType']: t['VCpuInfo']['DefaultVCpus'] for t in data['InstanceTypes']})
    return bound(config, cpus[config['host_type']], cpus[config['loader_type']], quota,
                 sum(cpus[i['InstanceType']] for i in other),
                 config.get('host_price') or price(region, config['host_type']),
                 config.get('loader_price') or price(region, config['loader_type']))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='command', required=True)
    initial = commands.add_parser('plan')
    for name in ('run', 'region', 'bucket', 'host-type', 'loader-type'):
        initial.add_argument('--' + name, required=True)
    for name in ('hosts', 'loaders', 'ttl'):
        initial.add_argument('--' + name, required=True, type=positive)
    initial.add_argument('--slots', type=int, required=True)
    initial.add_argument('--budget', required=True)
    initial.add_argument('--resource-slots', action='store_true',
                         help='slots is the address range; live machine resources admit each vnode')
    initial.add_argument('--max-hosts', type=positive)
    initial.add_argument('--min-peak-hosts', type=positive, help='fail if the planned fleet cannot reach this size')
    initial.add_argument('--host-price')
    initial.add_argument('--loader-price')
    update = commands.add_parser('update')
    update.add_argument('state', type=Path)
    update.add_argument('--budget')
    update.add_argument('--max-hosts', help='positive count, or auto to remove the operator ceiling')
    update.add_argument('--dry-run', action='store_true')
    args = vars(parser.parse_args())
    command = args.pop('command')
    if command == 'plan':
        result = plan(args)
    else:
        state, dry = args.pop('state'), args.pop('dry_run')
        previous = json.loads(state.read_text())
        config = {k: previous[k] for k in ('run', 'region', 'bucket', 'host_type', 'loader_type',
                  'hosts', 'slots', 'loaders', 'ttl', 'budget', 'max_hosts', 'host_price', 'loader_price')}
        if previous.get('min_peak_hosts') is not None:
            config['min_peak_hosts'] = previous['min_peak_hosts']
        if previous.get('resource_slots'):
            config['resource_slots'] = True
        for k, v in args.items():
            if v is not None:
                config[k] = None if k == 'max_hosts' and v == 'auto' else positive(v) if k == 'max_hosts' else v
        result = plan(config)
        if not dry:
            encoded = json.dumps(result, indent=2).encode()
            subprocess.run(['aws', '--region', result['region'], 's3', 'cp', '-',
                            f"s3://{result['bucket']}/plan/capacity.json", '--only-show-errors'],
                           input=encoded, check=True)
            temporary = state.with_suffix('.tmp')
            temporary.write_bytes(encoded)
            os.replace(temporary, state)
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    try:
        main()
    except (ValueError, KeyError, InvalidOperation, OSError, subprocess.CalledProcessError) as error:
        sys.exit(f'capacity: {error}')
