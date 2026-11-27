// Own all local node processes, including those requested by the Go provisioner.
// Clients, the issuer and the public gateway cannot reach this Unix-domain socket.
import { spawn, execFileSync } from 'node:child_process';
import { createServer } from 'node:http';
import { mkdir, readFile, readdir, rename, rm, writeFile, access } from 'node:fs/promises';
import { join } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

const data = process.env.INDEXUS_DATA || '/data';
const root = process.env.INDEXUS_ROOT || '/opt/indexus';
const node = join(root, 'bin/indexus-node');
const network = process.env.INDEXUS_NETWORK || 'indexus-quickstart';
const socket = process.env.INDEXUS_SLOT_SOCKET || '/tmp/indexus-vnodes.sock';
function setting(key, fallback, max) {
  const n = Number(process.env[key] || fallback);
  if (!Number.isSafeInteger(n) || n < 1 || n > max) throw Error(key + ' outside 1..' + max);
  return n;
}
const limit = setting('INDEXUS_MAX_VNODES', 1, 64);
const items = setting('INDEXUS_ITEMS_PER_VNODE', 1000000, Number.MAX_SAFE_INTEGER);
const demand = setting('INDEXUS_WRITES_PER_VNODE', 50, Number.MAX_SAFE_INTEGER);
const children = new Map();
let stopping = false;
let starting = false;
let server;

async function shutdown(code) {
  if (stopping) return;
  stopping = true;
  server?.close();
  await Promise.all([...children.values()].map(child => new Promise(resolve => {
    child.once('exit', resolve);
    child.kill('SIGTERM');
  })));
  process.exit(code);
}
process.once('SIGTERM', () => shutdown(0));
process.once('SIGINT', () => shutdown(0));

async function start(slot, target) {
  const cwd = join(data, 'node' + slot);
  const path = join(cwd, 'node.json');
  const port = 21030 + slot;
  const monitor = 19030 + slot;
  await mkdir(cwd, { recursive: true });
  let config;
  try { config = JSON.parse(await readFile(path, 'utf8')); }
  catch (e) {
    if (e.code !== 'ENOENT') throw e;
    if (slot !== 1 && !target) throw Error('missing saved identity configuration for node' + slot);
    config = {
      compaction: false,
      storage: { path: 'node', identity_directory: '.', omit_history: true },
      identity: { network, cluster: network, issuer: 'http://127.0.0.1:22030',
        ...(slot === 1 ? { name: '0000000000000001' } : { prefer_near: target }) },
      network: { listen: '127.0.0.1:' + port, monitoring: '127.0.0.1:' + monitor, advertise: ['127.0.0.1'],
        ...(slot === 1 ? {} : { bootstrap: ['http://127.0.0.1:21031'] }) },
      limits: { data_bytes: 8 * 2 ** 30, memory_refuse_pct: 60 },
    };
  }
  config.provisioner = limit > 1 ? 'http://127.0.0.1:22010' : '';
  config.scaling = { items_per_vnode: items, writes_per_vnode: demand, resource_signals: false };
  await writeFile(path + '.next', JSON.stringify(config, null, 2));
  await rename(path + '.next', path);
  if (slot === 1) {
    try { await access(join(cwd, 'node.causal-identity.json')); }
    catch (e) {
      if (e.code !== 'ENOENT') throw e;
      execFileSync(node, ['init', '-config', path], { cwd, stdio: 'inherit' });
    }
  }
  const child = spawn(node, ['serve', '-config', path], { cwd, stdio: 'inherit',
    env: { GOMAXPROCS: '2', GOMEMLIMIT: '512MiB', ...process.env } });
  children.set(slot, child);
  child.once('error', e => { console.error(e); children.delete(slot); shutdown(1); });
  child.once('exit', (code, signal) => {
    children.delete(slot);
    if (!stopping) { console.error('vnode exited', slot, code, signal); shutdown(1); }
  });
  for (let i = 0; i < 480; i++) {
    if (stopping) throw Error('stopping');
    try {
      const r = await fetch('http://127.0.0.1:' + monitor + '/health', { signal: AbortSignal.timeout(1000) });
      await r.body?.cancel();
      if (r.ok) return;
    } catch { /* The server has not bound its monitor yet. */ }
    await delay(250);
  }
  throw Error('node' + slot + ' did not become healthy');
}

async function main() {
  // Never strand rights on the disks of a previous three-node demo or a
  // previous, larger limit. The limit only applies to NEW processes.
  const saved = [];
  for (const name of await readdir(data)) {
    if (!/^node[1-9][0-9]*$/.test(name)) continue;
    const files = await readdir(join(data, name));
    // An interrupted mkdir/config rename precedes identity creation. No
    // rights exist on an empty slot. A nonempty damaged store fails closed.
    if (files.some(file => file !== 'node.json.next')) saved.push(Number(name.slice(4)));
  }
  const slots = [...new Set([1, ...saved])].sort((a, b) => a - b);
  for (const slot of slots) await start(slot);
  await rm(socket, { force: true });
  server = createServer(async (req, res) => {
    if (req.method === 'GET' && req.url === '/count') return res.end(String(children.size - 1));
    if (req.method === 'GET' && req.url === '/endpoints')
      return res.end([...children.keys()].map(i => 'http://127.0.0.1:' + (21030 + i)).join(','));
    const url = new URL(req.url, 'http://localhost');
    if (req.method !== 'POST' || url.pathname !== '/spawn') { res.writeHead(404); return res.end(); }
    if (starting || stopping || children.size >= limit) { res.writeHead(503); return res.end(); }
    const target = url.searchParams.get('near');
    if (!target || target.length !== 16) { res.writeHead(400); return res.end('invalid target'); }
    starting = true;
    const slot = Math.max(...children.keys()) + 1;
    try {
      await start(slot, target);
      res.end('local-' + slot);
    } catch (e) {
      res.writeHead(500); res.end(String(e)); console.error(e); shutdown(1);
    } finally { starting = false; }
  });
  server.listen(socket);
}
main().catch(e => { console.error(e); shutdown(1); });
