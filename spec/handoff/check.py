#!/usr/bin/env python3
"""Run bounded models and expected counterexamples with TLC."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--jar', type=Path, required=True)
parser.add_argument('--out', type=Path, required=True)
parser.add_argument('--timeout', type=float, default=600,
                    help='maximum seconds per model (default: 600)')
parser.add_argument('--only', action='append', default=[], metavar='CONFIG',
                    help='run only this configuration; repeat to select several')
args = parser.parse_args()
if args.timeout <= 0:
    parser.error('--timeout must be positive')
jar = args.jar.resolve()
expected_sha = '936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88'
if hashlib.sha256(jar.read_bytes()).hexdigest() != expected_sha:
    raise SystemExit('Use pinned tla2tools.jar v1.7.4; see README.md')
args.out.mkdir(parents=True, exist_ok=True)
results = []
models = [('ResumeIsolation', 'ResumeIsolation', 0, 'Model checking completed. No error has been found.'),
          ('IncomingBlocksOutgoing', 'ResumeIsolation', 13, 'Temporal properties were violated.'),
          ('ReceiptAfterDeparture', 'ReceiptAfterDeparture', 0, 'Model checking completed. No error has been found.'),
          ('DepartureBeforeDrain', 'ReceiptAfterDeparture', 12, 'Invariant SoundReceipt is violated.'),
          ('AddressAsDrain', 'ReceiptAfterDeparture', 12, 'Invariant SoundReceipt is violated.'),
          ('FirstFenceCancellation', 'FirstFenceCancellation', 0, 'Model checking completed. No error has been found.'),
          ('CancelAfterPointFence', 'FirstFenceCancellation', 12, 'Invariant SingleWriter is violated.'),
          ('GranularRecovery', 'GranularRecovery', 0, 'Model checking completed. No error has been found.'),
                              ('MetadataBlocked', 'GranularRecovery', 13, 'Temporal properties were violated.'),
                              ('UnsafeThaw', 'GranularRecovery', 12, 'Invariant SingleWriter is violated.'),
                              ('SubregionAuthority', 'SubregionAuthority', 0, 'Model checking completed. No error has been found.'),
                              ('UnsafeReselect', 'SubregionAuthority', 12, 'Invariant SingleWriter is violated.'),
                              ('TokenIsolation', 'TokenIsolation', 0, 'Model checking completed. No error has been found.'),
                              ('SerialDisk', 'TokenIsolation', 13, 'Temporal properties were violated.'),
                              ('AuthorityCompaction', 'AuthorityCompaction', 0, 'Model checking completed. No error has been found.'),
                              ('CompactionReplay', 'AuthorityCompaction', 12, 'Invariant SingleWriter is violated.'),
                              ('CompactionRestore', 'AuthorityCompaction', 12, 'Invariant SingleWriter is violated.'),
                              ('CoalescedRights', 'CoalescedRights', 0, 'Model checking completed. No error has been found.'),
                              ('UnsafeCoalesce', 'CoalescedRights', 12, 'Invariant SingleWriter is violated.'),
                              ('InstallReceipt', 'InstallReceipt', 0, 'Model checking completed. No error has been found.'),
                              ('ForgottenInstallReceipt', 'InstallReceipt', 13, 'Temporal properties were violated.'),
                              ('ReadSuccessors', 'ReadSuccessors', 0, 'Model checking completed. No error has been found.'),
                              ('ForgottenReadSuccessor', 'ReadSuccessors', 12, 'Invariant CompleteRead is violated.'),
                              ('OneHopRead', 'ReadSuccessors', 12, 'Invariant CompleteRead is violated.'),
                              ('StaleReturnSuccessor', 'ReadSuccessors', 12, 'Invariant NoCycles is violated.'),
                              ('ReceiverTickets', 'ReceiverTickets', 0, 'Model checking completed. No error has been found.'),
                              ('TicketsReplay', 'ReceiverTickets', 12, 'Invariant SingleWriter is violated.'),
                              ('TicketsForgetOpen', 'ReceiverTickets', 12, 'Invariant SoundACK is violated.'),
                              ('TicketsNoFence', 'ReceiverTickets', 12, 'Invariant SingleWriter is violated.'),
                              ('TicketData', 'TicketData', 0, 'Model checking completed. No error has been found.'),
                              ('TicketDataNoCopy', 'TicketData', 12, 'Invariant WriterHasLatest is violated.'),
                              ('TicketDataEarlyCollect', 'TicketData', 12, 'Invariant ACKRetained is violated.'),
                              ('TicketDataOldCollector', 'TicketData', 12, 'Invariant WriterHasLatest is violated.')]
unknown = set(args.only) - {config for config, *_ in models}
if unknown:
    parser.error('unknown configuration(s): ' + ', '.join(sorted(unknown)))
for config, model, code, marker in models:
    if args.only and config not in args.only:
        continue
    command = ['java', '-XX:+UseParallelGC', '-Xmx2g', '-cp', str(jar), 'tlc2.TLC',
               '-workers', '1', '-lncheck', 'final', '-metadir', str((args.out/config).resolve()),
               '-config', config+'.cfg', model+'.tla']
    try:
        result = subprocess.run(command, cwd=Path(__file__).parent, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                timeout=args.timeout)
    except subprocess.TimeoutExpired as exc:
        output = exc.stdout or b''
        if isinstance(output, bytes):
            output = output.decode(errors='replace')
        (args.out/(config+'.log')).write_text(output)
        raise SystemExit(f'{config}: timeout after {args.timeout:g}s; inspect log') from exc
    (args.out/(config+'.log')).write_text(result.stdout)
    if result.returncode != code or marker not in result.stdout:
        raise SystemExit(f'{config}: unexpected TLC result; inspect log')
    states = re.findall(r'([\d,]+) distinct states found', result.stdout)
    results.append(dict(model=config, exit_code=result.returncode,
                        distinct_states=int(states[-1].replace(',','')) if states else None,
                        result='configured_checks_passed' if code==0 else 'expected_counterexample'))
summary = dict(tool='TLC 2.19', tools_release='v1.7.4', sha256=expected_sha, workers=1,
               timeout_seconds=args.timeout, checks=results)
(args.out/'results.json').write_text(json.dumps(summary, indent=2))
print(json.dumps(summary, indent=2))
