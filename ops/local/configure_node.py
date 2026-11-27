#!/usr/bin/env python3
"""Write a lab node's canonical JSON configuration. No legacy flags are accepted."""
import json, os, sys
from pathlib import Path
output, storage, identity, name, p2p, monitoring, bootstrap = sys.argv[1:]
config = dict(
 storage=dict(path=storage, identity_directory=identity),
 identity=dict(network=os.environ['NETWORK_ID'], cluster='indexus', issuer=os.environ['ISSUER_URL']),
 network=dict(listen=f'127.0.0.1:{int(p2p)}', monitoring=f'127.0.0.1:{int(monitoring)}', advertise=['127.0.0.1']),
 limits=dict(data_bytes=32 << 30, memory_refuse_pct=float(os.environ.get('MEM_REFUSE_PCT', '60'))),
 provisioner='http://127.0.0.1:22010',
)
if name: config['identity']['prefer_near'] = name  # a target: each spawn gets a fresh name
if bootstrap: config['network']['bootstrap'] = [bootstrap]
if os.environ.get('INDEXUS_P2P_TLS') == '1': config['network']['tls_directory'] = os.environ['INDEXUS_TLS_DIR']
Path(output).write_text(json.dumps(config, indent=2)+'\n')
