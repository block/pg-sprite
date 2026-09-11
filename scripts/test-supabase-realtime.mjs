#!/usr/bin/env node
// Node 22+: uses its built-in WebSocket and the localhost compose fixture.
// Wire protocol: https://supabase.com/docs/guides/realtime/protocol
import assert from 'node:assert/strict';
import { createHmac } from 'node:crypto';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { resolve, dirname } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const compose = ['compose', '-p', 'pgsprite-supabase', '-f', resolve(root, 'compose/supabase.yml')];
const table = 'public.pgsprite_realtime_probe';
const children = new Set();
const subscribers = [];
let closing = false;
let failure;

function command(program, args, input = '', onData = () => {}) {
  return new Promise((ok, fail) => {
    const child = spawn(program, args, { cwd: root, stdio: ['pipe', 'pipe', 'pipe'] });
    children.add(child);
    let stdout = '', stderr = '';
    const timeout = setTimeout(() => child.kill('SIGKILL'), 60000);
    child.stdout.on('data', data => { stdout += data; onData(stdout); });
    child.stderr.on('data', data => { stderr += data; });
    child.on('error', fail);
    child.on('close', code => {
      clearTimeout(timeout);
      children.delete(child);
      code === 0 ? ok(stdout.trim()) : fail(new Error(`${program} exited ${code}: ${stderr}`));
    });
    child.stdin.end(input);
  });
}
const sql = (text, onData) => command('docker', [...compose, 'exec', '-T', 'postgres', 'psql', '-U', 'postgres', '-d', 'postgres', '-qAt', '-v', 'ON_ERROR_STOP=1'], text, onData);

async function until(check, label) {
  const deadline = Date.now() + 30000;
  while (Date.now() < deadline) {
    if (failure) throw failure;
    if (await check()) return;
    await delay(50);
  }
  throw new Error(`Timed out: ${label}`);
}

function jwt(tenant) {
  const encode = value => Buffer.from(JSON.stringify(value)).toString('base64url');
  const body = encode({ alg: 'HS256', typ: 'JWT' }) + '.' + encode({ role: 'authenticated', sub: `00000000-0000-0000-0000-${String(tenant).padStart(12, '0')}`, exp: Math.floor(Date.now()/1000) + 600 });
  return body + '.' + createHmac('sha256', 'pgsprite-local-test-jwt-secret-not-for-production').update(body).digest('base64url');
}

function subscribe(tenant) {
  const token = jwt(tenant);
  const topic = `realtime:pgsprite-tenant-${tenant}`;
  const ws = new WebSocket(`ws://localhost:55442/socket/websocket?apikey=${token}&vsn=1.0.0`);
  const state = { ws, ready: false, records: new Map() };
  subscribers.push(state);
  let ref = 1;
  const send = (event, payload, target = topic) => ws.send(JSON.stringify({ topic: target, event, payload, ref: String(ref++), join_ref: target === topic ? '1' : null }));
  ws.onopen = () => send('phx_join', { config: { postgres_changes: [{ event: '*', schema: 'public', table: 'pgsprite_realtime_probe' }] }, access_token: token });
  state.heartbeat = setInterval(() => { if (ws.readyState === WebSocket.OPEN) send('heartbeat', {}, 'phoenix'); }, 10000);
  ws.onerror = () => { failure = new Error(`Tenant ${tenant}: WebSocket error`); };
  ws.onclose = event => { if (!closing) failure = new Error(`Tenant ${tenant}: socket closed ${event.code}`); };
  ws.onmessage = event => {
    try {
      const msg = JSON.parse(event.data);
      if (msg.event === 'phx_error' || msg.event === 'phx_close' || msg.payload?.status === 'error') throw new Error(`Subscription failed: ${JSON.stringify(msg)}`);
      if (msg.event === 'system' && msg.payload.extension === 'postgres_changes' && msg.payload.status === 'ok') state.ready = true;
      if (msg.event !== 'postgres_changes') return;
      const data = msg.payload.data;
      assert.equal(data.record.owner_id, `00000000-0000-0000-0000-${String(tenant).padStart(12, '0')}`, 'cross-tenant event');
      state.records.set(`${data.type}:${data.record.id}`, data.record);
    } catch (err) { failure = err; }
  };
  return state;
}

async function change(statement) {
  // All URLs are fixed disposable fixture addresses; no external project is accepted.
  const report = JSON.parse(await command(resolve(root, 'bin/pg-sprite'), ['migrate', '--url', 'postgres://postgres.pgsprite:pgsprite_test_only@127.0.0.1:55440/postgres?sslmode=disable', '--alter', statement, '--json']));
  assert.equal(report.outcome, 'executed-natively');
  return report;
}

async function main() {
  await sql(`DROP TABLE IF EXISTS ${table};
CREATE TABLE ${table} (id int PRIMARY KEY, owner_id uuid NOT NULL, body text NOT NULL);
ALTER TABLE ${table} ENABLE ROW LEVEL SECURITY;
CREATE POLICY own_rows ON ${table} TO authenticated USING (owner_id=auth.uid());
GRANT SELECT ON ${table} TO authenticated;
INSERT INTO ${table} SELECT n+100000, '00000000-0000-0000-0000-000000000003', md5(n::text) FROM generate_series(1,100000) n;
ALTER PUBLICATION supabase_realtime ADD TABLE ${table};`);
  try {
    // Recreating the fixture changes its OID. Realtime caches publication OIDs
    // for 60 seconds; reset that test state before subscribing, never during DDL.
    await command('docker', [...compose, 'up', '-d', '--no-deps', '--force-recreate', 'realtime']);
    await until(async () => {
      try {
        await command('docker', [...compose, 'exec', '-T', 'realtime', 'test', '-f', '/tmp/pgsprite-server-ready']);
        const response = await fetch('http://localhost:55442/api/tenants/localhost/health', {
          headers: { Authorization: `Bearer ${jwt(1)}` },
          signal: AbortSignal.timeout(2000),
        });
        await response.arrayBuffer();
        return response.ok;
      } catch { return false; }
    }, 'Realtime health');
    const originalOID = await sql(`SELECT '${table}'::regclass::oid;`);
    const first = subscribe(1), second = subscribe(2);
    await until(() => first.ready && second.ready, 'both subscriptions ready');
    await sql(`INSERT INTO ${table} VALUES (0,'00000000-0000-0000-0000-000000000001','before'),(1000,'00000000-0000-0000-0000-000000000002','before');`);
    await until(() => first.records.has('INSERT:0') && second.records.has('INSERT:1000'), 'baseline events');

    let committed = 0;
    const writes = [];
    for (let n = 1; n <= 60; n++) {
      writes.push(`INSERT INTO ${table}(id,owner_id,body) VALUES (${n},'00000000-0000-0000-0000-000000000001','insert'),(${n+1000},'00000000-0000-0000-0000-000000000002','insert');
UPDATE ${table} SET body='updated' WHERE id IN (${n},${n+1000});
SELECT 'committed:${n}';
SELECT pg_sleep(0.05);`);
    }
    const writer = sql(writes.join('\n'), output => {
      const matches = [...output.matchAll(/committed:(\d+)/g)];
      if (matches.length) committed = Number(matches.at(-1)[1]);
    }).catch(err => { failure = err; });
    await until(() => committed >= 2, 'writer active');
    const atStart = committed;
    await change(`ALTER TABLE ${table} ADD COLUMN title text`);
    const report = await change(`CREATE INDEX pgsprite_realtime_probe_body ON ${table}(body)`);
    assert.match(report.executed_sql[0], /^CREATE INDEX CONCURRENTLY /);
    assert(committed > atStart, 'no writes committed during schema changes');
    assert(committed < 60, 'writer finished before schema changes did');
    await writer;
    if (failure) throw failure;
    await sql(`INSERT INTO ${table} VALUES (999,'00000000-0000-0000-0000-000000000001','after','new column'),(1999,'00000000-0000-0000-0000-000000000002','after','new column');`);
    await until(() => first.records.size === 122 && second.records.size === 122, 'all 244 expected events');
    for (const [subscriber, offset] of [[first, 0], [second, 1000]]) {
      for (let n=1; n<=60; n++) {
        assert.equal(subscriber.records.get(`INSERT:${n+offset}`).body, 'insert');
        assert.equal(subscriber.records.get(`UPDATE:${n+offset}`).body, 'updated');
      }
      assert.equal(subscriber.records.get(`INSERT:${999+offset}`).title, 'new column');
    }
    assert.equal(await sql(`SELECT count(*) FROM ${table};`), '100124');
    assert.equal(await sql(`SELECT '${table}'::regclass::oid;`), originalOID);
    assert.equal(await sql(`SELECT relrowsecurity FROM pg_class WHERE oid='${table}'::regclass;`), 't');
    assert.equal(await sql(`SELECT count(*) FROM pg_publication_tables WHERE pubname='supabase_realtime' AND tablename='pgsprite_realtime_probe';`), '1');
    assert.equal(await sql(`SELECT indisvalid FROM pg_index WHERE indexrelid='public.pgsprite_realtime_probe_body'::regclass;`), 't');
    console.log('PASS: same two sockets, 244 expected INSERT/UPDATE events, concurrent writes, new-column payload, tenant isolation, table identity and publication preserved');
  } finally {
    closing = true;
    for (const subscriber of subscribers) { clearInterval(subscriber.heartbeat); subscriber.ws.close(); }
    for (const child of children) child.kill('SIGKILL');
    await sql(`DROP TABLE IF EXISTS ${table};`);
  }
}
main().catch(err => { console.error(err); process.exitCode = 1; });
