import assert from 'node:assert/strict';
import { execFile as nativeExecFile } from 'node:child_process';
import { EventEmitter } from 'node:events';
import { chmodSync, existsSync, mkdtempSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, isAbsolute, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import test from 'node:test';
import receiver, { installReceiver } from './pi-receiver.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const receiptId = '1'.repeat(64);
const secondId = '2'.repeat(64);
const ackedAt = '2026-11-26T01:03:00.123456+00:00';
const pause = (ms = 15) => new Promise(resolve => setTimeout(resolve, ms));

async function until(predicate, message = 'condition', timeout = 2500) {
  const deadline = Date.now() + timeout;
  while (!predicate()) {
    assert.ok(Date.now() < deadline, `Timed out: ${message}`);
    await pause();
  }
}

function receipt(recipient = 'session-a', updates = {}) {
  return {
    receipt_id: receiptId, recipient, state: 'delivered', acked_at: null,
    payload: {
      repo: 'example/project', sha: 'a'.repeat(40), run_id: 91, attempt: 2,
      action: 'first_failure', job: '501', job_id: 501, conclusion: 'failure',
      job_url: 'https://github.com/example/project/actions/runs/91/job/501',
      tests: [{ name: 'TestRetry/sub', package: 'example/store' }],
    },
    ...updates,
  };
}

function tagReceipt(recipient = 'session-a', updates = {}) {
  const tag = 'refs/tags/rc/候选-$(do-not-run)';
  const tagObject = 'b'.repeat(40);
  return receipt(recipient, {
    payload: {
      repo: 'example/project', sha: 'a'.repeat(40), run_id: null, attempt: null,
      action: 'release_tag_forward', job: `@tag:${tag}:${tagObject}`,
      tag, tag_object: tagObject, tested_sha: null,
    },
    ...updates,
  });
}

function fixture(t) {
  const dir = mkdtempSync(join(tmpdir(), 'ci-pi-receiver-'));
  const db = join(dir, 'observer.db');
  writeFileSync(db, 'database fixture', { mode: 0o600 });
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return { dir, db };
}

function fakePi(dir, { id = 'session-a', entries = [], persist = true, mode = 'rpc' } = {}) {
  const sessionFile = join(dir, `${id}.jsonl`);
  writeFileSync(sessionFile, `${JSON.stringify({ type: 'session', version: 3, id })}\n`, { mode: 0o600 });
  const handlers = new Map();
  const messages = [];
  const notices = [];
  const tools = [];
  const manager = {
    getSessionId: () => id,
    getSessionFile: () => sessionFile,
    getEntries: () => entries,
    getBranch: () => entries,
  };
  const ctx = { mode, hasUI: true, sessionManager: manager, ui: { notify: (...args) => notices.push(args) } };
  const pi = {
    on(name, handler) { handlers.set(name, handler); },
    registerTool(tool) { tools.push(tool); },
    appendEntry(customType, data) { entries.push({ type: 'custom', customType, data: structuredClone(data) }); },
    sendMessage(message, options) {
      messages.push({ message, options });
      if (persist) persistMessage(message);
    },
  };
  function persistMessage(message) {
    entries.push({ type: 'custom_message', ...structuredClone(message), id: `entry-${entries.length}` });
  }
  return {
    pi, ctx, entries, messages, notices, tools, sessionFile, manager, persistMessage,
    emit: (name, event = {}, context = ctx) => handlers.get(name)?.(event, context),
  };
}

function fakeIO(rows = [], { ignoreAbort = false, response } = {}) {
  const calls = [];
  const watches = [];
  let running = 0;
  let peak = 0;
  const io = {
    rows, calls, watches,
    get peak() { return peak; },
    watch(path, options, callback) {
      if (typeof options === 'function') callback = options;
      const watcher = new EventEmitter();
      Object.assign(watcher, { path, callback, closed: false, close() { this.closed = true; }, unref() {} });
      watches.push(watcher);
      return watcher;
    },
    execFile(command, args, options, callback) {
      const call = { command, args, options, killed: false };
      calls.push(call);
      running++;
      peak = Math.max(peak, running);
      let finished = false;
      function finish(error, stdout) {
        if (finished) return;
        finished = true;
        running--;
        options.signal?.removeEventListener('abort', abort);
        callback(error, stdout ?? '', error?.stderr ?? 'secret stderr must never be forwarded');
      }
      function abort() {
        call.killed = true;
        if (!ignoreAbort) finish(new Error('aborted'));
      }
      options.signal?.addEventListener('abort', abort, { once: true });
      const child = { kill() { abort(); return true; } };
      queueMicrotask(async () => {
        try {
          const subcommand = args[args.indexOf('--db') + 2];
          const recipient = args[args.indexOf('--recipient') + 1];
          if (response) {
            const output = await response(subcommand, recipient, call);
            if (output !== undefined) { finish(null, output); return; }
          }
          if (subcommand === 'ack') {
            const id = args[args.indexOf('--receipt') + 1];
            const row = io.rows.find(row => row.recipient === recipient && row.receipt_id === id);
            if (!row || row.state === 'pending') throw new Error('not delivered');
            row.state = 'acked';
            row.acked_at ??= ackedAt;
            finish(null, '{"ok":true}');
          } else {
            let selected = [...new Map(io.rows.filter(row => row?.recipient === recipient && /^[a-f0-9]{64}$/.test(row.receipt_id)).map(row => [row.receipt_id, row])).values()];
            if (args.includes('--receipt')) selected = selected.filter(row => row.receipt_id === args[args.indexOf('--receipt') + 1]);
            if (args.includes('--pending')) {
              selected = selected.filter(row => row.state === 'delivered' && row.acked_at == null);
              selected.sort((a, b) => a.receipt_id.localeCompare(b.receipt_id));
              if (args.includes('--after')) selected = selected.filter(row => row.receipt_id > args[args.indexOf('--after') + 1]);
              selected = selected.slice(0, Number(args[args.indexOf('--limit') + 1]));
            }
            call.returnedRows = selected.length;
            finish(null, JSON.stringify(selected));
          }
        } catch (error) { finish(error); }
      });
      return child;
    },
    event(name = 'observer.db-wal') {
      for (const watcher of watches) if (!watcher.closed) watcher.callback('change', name);
    },
  };
  return io;
}

function install(t, f, pi, io, extra = {}) {
  installReceiver(pi.pi, {
    env: { CI_OBSERVER_DB: f.db, PI_SESSION_ID: 'forged-env-actor' },
    watch: io.watch, execFile: io.execFile, debounceMs: 5, reconcileMs: 60000, ...extra,
  });
  t.after(() => pi.emit('session_shutdown'));
}

async function begin(pi, args = { receiptId }, context = pi.ctx, toolCallId = 'call_fixture|fc_fixture') {
  const tool = pi.tools.find(tool => tool.name === 'ci_observer_begin_receipt');
  assert.ok(tool);
  return tool.execute(toolCallId, args, new AbortController().signal, undefined, context);
}

const ackCalls = io => io.calls.filter(call => call.args.includes('ack'));
const inboxCalls = io => io.calls.filter(call => call.args.includes('inbox'));
const dataFor = message => JSON.parse(message.content.slice(message.content.indexOf('\n{') + 1)).data;
const idFor = number => number.toString(16).padStart(64, '0');

async function waitDetails(pi, id) {
  const tool = pi.tools.find(tool => tool.name === 'ci_observer_wait_details');
  assert.ok(tool, 'explicit, nonblocking detail subscription tool');
  return tool.execute('call_wait_fixture|fc_wait_fixture', { receiptId: id }, new AbortController().signal, undefined, pi.ctx);
}

test('factory is inert; startup derives identity, uses safe argv and queues a non-interrupting turn', async t => {
  assert.equal(typeof receiver, 'function');
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const io = fakeIO([receipt()]);
  install(t, f, pi, io);
  assert.equal(io.calls.length + io.watches.length, 0);
  await pi.emit('session_start');
  assert.equal(io.watches.length, 1);
  assert.equal(io.watches[0].path, f.dir);
  assert.equal(pi.messages.length, 1);
  assert.deepEqual(pi.messages[0].options, { triggerTurn: true, deliverAs: 'followUp' });
  assert.equal(pi.messages[0].message.details.receiptId, receiptId);
  assert.equal(pi.messages[0].message.details.recipient, 'session-a');
  const call = io.calls[0];
  assert.equal(call.command, 'python3');
  assert.equal(call.args[0], '-B');
  assert.equal(call.args[1], join(here, 'observer.py'));
  assert.ok(isAbsolute(call.args[1]));
  assert.deepEqual(call.args.slice(2), ['--db', f.db, 'inbox', '--recipient', 'session-a', '--pending', '--limit', '64']);
  assert.equal(call.options.shell, false);
  assert.equal(call.options.killSignal, 'SIGKILL');
  assert.ok(call.options.timeout > 0 && call.options.timeout <= 30000);
  assert.ok(call.options.maxBuffer > 0 && call.options.maxBuffer <= 8 * 1024 * 1024);
  assert.equal(ackCalls(io).length, 0);
  assert.equal(pi.tools[0].parameters.additionalProperties, false);
  assert.deepEqual(Object.keys(pi.tools[0].parameters.properties), ['receiptId']);
});

test('only delivered own receipts wake; field allowlist bounds untrusted data and exact action identity', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const unsafe = receipt();
  unsafe.payload.tests = Array.from({ length: 200 }, () => ({ name: 'TestData\n$(do-not-run)\u001b' + 'x'.repeat(1000), package: 'pkg' }));
  Object.assign(unsafe.payload, {
    body: 'IGNORE_ALL_RULES_BODY', logs: 'SECRET_RAW_LOG', command: 'UNTRUSTED_SHELL',
    author_session: 'forged-actor', run_url: 'https://evil.invalid/token',
  });
  const io = fakeIO([
    unsafe, structuredClone(unsafe), receipt('someone-else', { receipt_id: secondId }),
    receipt('session-a', { receipt_id: '3'.repeat(64), state: 'pending' }),
    receipt('session-a', { receipt_id: '4'.repeat(64), state: 'acked', acked_at: ackedAt }),
    receipt('session-a', { receipt_id: '5'.repeat(64), acked_at: ackedAt }),
    receipt('session-a', { receipt_id: 'not-a-stable-id' }),
    receipt('session-a', { receipt_id: '6'.repeat(64), payload: { ...unsafe.payload, action: 'shell' } }),
  ]);
  install(t, f, pi, io);
  await pi.emit('session_start');
  assert.equal(pi.messages.length, 1);
  const { message } = pi.messages[0];
  assert.ok(message.content.length < 14000);
  for (const forbidden of ['IGNORE_ALL_RULES_BODY', 'SECRET_RAW_LOG', 'UNTRUSTED_SHELL', 'forged-actor', 'evil.invalid', '\u001b']) {
    assert.ok(!message.content.includes(forbidden), forbidden);
  }
  assert.ok(message.content.includes('$(do-not-run)'));
  assert.match(message.content, /数据/);
  assert.match(message.content, /ci_observer_begin_receipt/);
  assert.deepEqual(message.details.actionIdentity, {
    repo: 'example/project', sha: 'a'.repeat(40), run_id: 91, attempt: 2, action: 'first_failure', job: '501',
  });
});

test('fixture: exact no-run RC tag receipt is data-only and asks an authorized release worker to recheck', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const row = tagReceipt();
  Object.assign(row.payload, { title: 'UNTRUSTED_TITLE', command: 'UNTRUSTED_COMMAND', run_url: 'https://evil.invalid' });
  const io = fakeIO([row]);
  install(t, f, pi, io);
  await pi.emit('session_start');
  assert.equal(pi.messages.length, 1);
  const { message, options } = pi.messages[0];
  assert.deepEqual(options, { triggerTurn: true, deliverAs: 'followUp' });
  assert.deepEqual(message.details.actionIdentity, {
    repo: row.payload.repo, sha: row.payload.sha, run_id: null, attempt: null,
    action: 'release_tag_forward', job: row.payload.job, tag: row.payload.tag, tag_object: row.payload.tag_object,
  });
  assert.ok(message.content.includes(JSON.stringify(row.payload.tag)));
  assert.ok(message.content.includes('"tested_sha": null'));
  assert.match(message.content, /release worker.*复核/);
  assert.match(message.content, /尚无.*GitHub run/);
  for (const forbidden of ['UNTRUSTED_TITLE', 'UNTRUSTED_COMMAND', 'evil.invalid', 'run_url', 'job_url', '/actions/runs/null']) {
    assert.ok(!message.content.includes(forbidden), forbidden);
  }
  assert.equal(ackCalls(io).length, 0);
  const result = await begin(pi);
  assert.equal(result.details.ackedAt, ackedAt);
  assert.equal(result.details.actionIdentity.tag_object, row.payload.tag_object);
  await pi.emit('session_shutdown');
  io.rows[0].state = 'delivered';
  io.rows[0].acked_at = null;
  const resumed = fakePi(f.dir, { entries: structuredClone(pi.entries) });
  install(t, f, resumed, io);
  await resumed.emit('session_start', { reason: 'resume' });
  assert.equal(resumed.messages.length, 0);
  resumed.entries[0].details.actionIdentity.tag_object = 'c'.repeat(40);
  await assert.rejects(begin(resumed), /身份/);
  assert.equal(ackCalls(io).length, 1);
});

test('fixture: tag data preserves the core 1024-character ref contract and valid Unicode/punctuation', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const tags = ['refs/tags/rc/' + 'x'.repeat(600), 'refs/tags/rc/候选-😀', 'refs/tags/rc/candidate]'];
  const rows = tags.map((tag, index) => {
    const row = tagReceipt('session-a', { receipt_id: (index + 10).toString(16).padStart(64, '0') });
    row.payload.tag = tag;
    row.payload.job = `@tag:${tag}:${row.payload.tag_object}`;
    return row;
  });
  install(t, f, pi, fakeIO(rows));
  await pi.emit('session_start');
  assert.deepEqual(pi.messages.map(({ message }) => message.details.actionIdentity.tag), tags);
});

test('fixture: malformed tag identities cannot relax run/attempt requirements for workflow receipts', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const variants = [
    { run_id: 91 }, { run_id: undefined }, { attempt: 1 }, { attempt: undefined },
    { tested_sha: 'a'.repeat(40) }, { tested_sha: undefined },
    { tag: 'refs/heads/master' }, { tag: 'refs/tags/rc/nested/tag' }, { tag: 'refs/tags/rc/' },
    { tag: 'refs/tags/rc/a..b' }, { tag: 'refs/tags/rc/.hidden' }, { tag: 'refs/tags/rc/a.lock' },
    { tag: 'refs/tags/rc/name\ncommand' }, { tag: 'refs/tags/rc/name with spaces' },
    { tag_object: 'invalid-object' }, { tag_object: null }, { sha: null },
    { job: '@tag:refs/tags/rc/another:' + 'b'.repeat(40) },
    { tag: 'refs/tags/rc/' + 'x'.repeat(1100) }, { tag: 'refs/tags/rc/lone-\ud800' },
  ].map(updates => {
    const row = tagReceipt();
    Object.assign(row.payload, updates);
    if ('tag' in updates || 'tag_object' in updates) row.payload.job = `@tag:${row.payload.tag}:${row.payload.tag_object}`;
    return row;
  });
  for (const action of ['first_failure', 'terminal', 'job_details', 'attribution', 'release_binary_ready']) {
    for (const invalid of [null, undefined, 0, -1, '91']) {
      variants.push(receipt('session-a', { payload: { ...receipt().payload, action, run_id: invalid } }));
      variants.push(receipt('session-a', { payload: { ...receipt().payload, action, attempt: invalid } }));
    }
    const forged = tagReceipt();
    Object.assign(forged.payload, { action, run_id: 91, attempt: 2 });
    variants.push(forged);
  }
  variants.forEach((row, index) => { row.receipt_id = (index + 10).toString(16).padStart(64, '0'); });
  const io = fakeIO(variants);
  install(t, f, pi, io);
  await pi.emit('session_start');
  assert.equal(pi.messages.length, 0);
  assert.equal(ackCalls(io).length, 0);
});

test('fixture: begin trace uses each runtime toolCallId, never model input, while sharing an idempotent ack', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const io = fakeIO([receipt()]);
  install(t, f, pi, io);
  await pi.emit('session_start');
  await assert.rejects(begin(pi, { receiptId, toolCallId: 'forged-input-id' }));
  await assert.rejects(begin(pi, { receiptId, source: 'forged-input-source' }));
  for (const badId of ['', 'call\nsecret', 'x'.repeat(513), { toolCallId: 'forged' }]) {
    await assert.rejects(begin(pi, { receiptId }, pi.ctx, badId));
  }
  assert.equal(ackCalls(io).length, 0);
  const ids = ['call_fixture_a|fc_fixture_a', 'call_fixture_b|fc_fixture_b'];
  const results = await Promise.all(ids.map(id => begin(pi, { receiptId }, pi.ctx, id)));
  assert.equal(ackCalls(io).length, 1);
  results.forEach((result, index) => {
    assert.equal(result.details.toolCallId, ids[index]);
    assert.equal(result.details.source, 'ci-observer-begin-receipt/v1');
    assert.equal(result.details.receiptId, receiptId);
    assert.equal(result.details.ackedAt, ackedAt);
  });
});

test('begin requires an actual session entry, not a queue; durable ack is explicit and idempotent', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir, { persist: false });
  const io = fakeIO([receipt()]);
  install(t, f, pi, io);
  await pi.emit('session_start');
  await assert.rejects(begin(pi), /实际|持久|投递/);
  assert.equal(ackCalls(io).length, 0);
  pi.persistMessage(pi.messages[0].message);
  const result = await begin(pi);
  assert.equal(result.details.ackedAt, ackedAt);
  assert.equal(result.details.recipient, 'session-a');
  assert.equal(ackCalls(io).length, 1);
  assert.deepEqual(ackCalls(io)[0].args.slice(-4), ['--recipient', 'session-a', '--receipt', receiptId]);
  const again = await begin(pi);
  assert.equal(again.details.ackedAt, result.details.ackedAt);
  assert.equal(ackCalls(io).length, 1);
});

test('begin compares action identity structurally and coalesces concurrent calls without changing ack time', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const io = fakeIO([receipt()]);
  install(t, f, pi, io);
  await pi.emit('session_start');
  const details = pi.entries[0].details;
  details.actionIdentity = Object.fromEntries(Object.entries(details.actionIdentity).reverse());
  const [first, second] = await Promise.all([begin(pi), begin(pi)]);
  assert.equal(first.details.ackedAt, ackedAt);
  assert.deepEqual(first, second);
  assert.equal(ackCalls(io).length, 1);
});

test('forged ids, extra actor parameters, pending receipts and fork-inherited entries cannot ack', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const io = fakeIO([receipt()]);
  install(t, f, pi, io);
  await pi.emit('session_start');
  await assert.rejects(begin(pi, { receiptId: secondId }));
  await assert.rejects(begin(pi, { receiptId, recipient: 'someone-else' }));
  await assert.rejects(begin(pi, { receiptId: `--receipt; touch /tmp/never` }));
  io.rows[0].state = 'pending';
  await assert.rejects(begin(pi));
  io.rows[0].state = 'delivered';
  io.rows[0].recipient = 'someone-else';
  await assert.rejects(begin(pi));
  io.rows[0].recipient = 'session-a';
  pi.entries[0].details.recipient = 'parent-session';
  await assert.rejects(begin(pi));
  assert.equal(ackCalls(io).length, 0);
});

test('persisted custom messages suppress restart/tree replay without auto-ack; queued-only restart retries', async t => {
  const f = fixture(t);
  const io = fakeIO([receipt()]);
  const first = fakePi(f.dir);
  install(t, f, first, io);
  await first.emit('session_start');
  await first.emit('session_tree');
  assert.equal(first.messages.length, 1);
  await first.emit('session_shutdown');
  const resumed = fakePi(f.dir, { entries: structuredClone(first.entries) });
  install(t, f, resumed, io);
  await resumed.emit('session_start', { reason: 'resume' });
  assert.equal(resumed.messages.length, 0);
  assert.equal(ackCalls(io).length, 0);
  await resumed.emit('session_shutdown');
  const queued = fakePi(f.dir, { persist: false });
  install(t, f, queued, io);
  await queued.emit('session_start');
  await queued.emit('session_shutdown');
  const retry = fakePi(f.dir, { entries: [] });
  install(t, f, retry, io);
  await retry.emit('session_start', { reason: 'startup' });
  assert.equal(retry.messages.length, 1);
  assert.equal(io.rows[0].acked_at, null);
});

test('watch bursts coalesce, reads serialize, irrelevant/unchanged events do not loop', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  let release;
  let count = 0;
  const io = fakeIO([], { response: async command => {
    if (command === 'inbox' && ++count === 2) await new Promise(resolve => { release = resolve; });
  } });
  install(t, f, pi, io);
  await pi.emit('session_start');
  for (let i = 0; i < 100; i++) io.event();
  io.event('unrelated.log');
  await pause(40);
  assert.equal(io.calls.length, 1);
  writeFileSync(f.db, 'changed database data');
  for (let i = 0; i < 100; i++) io.event('observer.db-journal');
  await until(() => release, 'second inbox read');
  io.rows.push(receipt());
  writeFileSync(f.db, 'newer database data containing receipt');
  for (let i = 0; i < 100; i++) io.event('observer.db');
  release();
  await until(() => pi.messages.length === 1, 'receipt delivery');
  await pause(60);
  assert.equal(io.peak, 1);
  assert.ok(io.calls.length <= 3);
  await pi.emit('session_shutdown');
  const before = io.calls.length;
  io.event();
  await pause(40);
  assert.equal(io.calls.length, before);
  assert.ok(io.watches.every(watcher => watcher.closed));
});

test('one low-frequency local reconcile recovers missed events; errors do not expose child stderr', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  let broken = true;
  const io = fakeIO([], { response: () => {
    if (broken) throw new Error('secret raw stdout/stderr: provider credential');
  } });
  install(t, f, pi, io, { reconcileMs: 35 });
  await pi.emit('session_start');
  assert.equal(pi.messages.length, 0);
  assert.ok(pi.notices.length <= 1);
  assert.ok(!JSON.stringify(pi.notices).includes('credential'));
  broken = false;
  io.rows.push(receipt());
  await until(() => pi.messages.length === 1, 'local reconcile');
  assert.ok(io.calls.every(call => call.command === 'python3' && !call.args.includes('reconcile')));
  await pi.emit('session_shutdown');
  const before = io.calls.length;
  await pause(100);
  assert.equal(io.calls.length, before);
});

test('shutdown kills children; stale asynchronous inbox results never enter a replacement session', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  let release;
  let delayed = true;
  const io = fakeIO([], { ignoreAbort: true, response: async (_command, recipient) => {
    if (delayed) {
      delayed = false;
      await new Promise(resolve => { release = resolve; });
    }
    return JSON.stringify([receipt(recipient)]);
  } });
  install(t, f, pi, io);
  const starting = pi.emit('session_start');
  await until(() => release, 'pending child');
  await pi.emit('session_shutdown', { reason: 'fork' });
  assert.ok(io.calls[0].killed);
  assert.ok(io.watches[0].closed);
  const fork = fakePi(f.dir, { id: 'session-b' });
  await pi.emit('session_start', { reason: 'fork' }, fork.ctx);
  release();
  await starting;
  assert.equal(pi.messages.length, 1);
  assert.equal(pi.messages[0].message.details.recipient, 'session-b');
  await assert.rejects(begin(pi, { receiptId }, pi.ctx));
});

test('no reception for missing/relative/untrusted DB, ephemeral/mismatched session or non-online modes', async t => {
  for (const variant of ['unset', 'relative', 'missing-db', 'public-db', 'symlink-db', 'public-dir', 'ephemeral', 'missing-session', 'mismatched', 'print', 'json']) {
    await t.test(variant, async t => {
      const f = fixture(t);
      const pi = fakePi(f.dir, { mode: ['print', 'json'].includes(variant) ? variant : 'rpc' });
      const io = fakeIO([receipt()]);
      let db = f.db;
      if (variant === 'unset') db = undefined;
      if (variant === 'relative') db = 'relative.db';
      if (variant === 'missing-db') db = join(f.dir, 'missing.db');
      if (variant === 'public-db') chmodSync(f.db, 0o644);
      if (variant === 'public-dir') chmodSync(f.dir, 0o755);
      if (variant === 'symlink-db') { db = join(f.dir, 'link.db'); symlinkSync(f.db, db); }
      if (variant === 'ephemeral') pi.manager.getSessionFile = () => undefined;
      if (variant === 'missing-session') rmSync(pi.sessionFile);
      if (variant === 'mismatched') writeFileSync(pi.sessionFile, '{"type":"session","id":"another-session"}\n');
      install(t, f, pi, io, { env: { CI_OBSERVER_DB: db } });
      await pi.emit('session_start');
      assert.equal(io.calls.length + io.watches.length + pi.messages.length, 0);
      if (variant === 'missing-db') assert.equal(existsSync(db), false);
    });
  }
});

test('malformed rows cannot block later valid receipts or supply non-string identities', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const malformed = receipt('session-a', { receipt_id: secondId });
  malformed.payload.action = { toString: null };
  const io = fakeIO([malformed, receipt()]);
  install(t, f, pi, io);
  await pi.emit('session_start');
  assert.equal(pi.messages.length, 1);
  assert.equal(pi.messages[0].message.details.receiptId, receiptId);
});

test('malformed inbox fails closed and does not ack', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const io = fakeIO([], { response: () => '{"command":"do not execute"}' });
  install(t, f, pi, io);
  await pi.emit('session_start');
  assert.equal(pi.messages.length, 0);
  assert.equal(ackCalls(io).length, 0);
});

test('fixture: 5000 acknowledged receipts never enter pending pages or begin lookups', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const history = Array.from({ length: 5000 }, (_, index) => receipt('session-a', {
    receipt_id: idFor(index + 1), state: 'acked', acked_at: ackedAt,
    payload: { ...receipt().payload, body: 'x'.repeat(2000) },
  }));
  const io = fakeIO([...history, receipt(), receipt('someone-else', { receipt_id: secondId })]);
  install(t, f, pi, io);
  await pi.emit('session_start');
  assert.equal(pi.messages.length, 1);
  await begin(pi);
  assert.ok(inboxCalls(io).every(call => call.args.includes('--pending') || call.args.includes('--receipt')));
  assert.ok(inboxCalls(io).every(call => call.returnedRows <= 1));
  assert.equal(inboxCalls(io).filter(call => call.args.includes('--pending')).length, 1);
});

test('fixture: pending pages advance with bounded chunks; lower-ID arrivals and resume neither starve nor replay', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const rows = Array.from({ length: 173 }, (_, index) => receipt('session-a', {
    receipt_id: idFor(index + 100), payload: { ...receipt().payload, run_id: index + 100 },
  }));
  let inserted = false;
  const io = fakeIO(rows, { response: (_command, _recipient, call) => {
    if (!inserted && call.args.includes('--after')) {
      inserted = true;
      io.rows.push(receipt('session-a', { receipt_id: idFor(1), payload: { ...receipt().payload, run_id: 1 } }));
      writeFileSync(f.db, 'new lower-ID receipt committed during a sweep');
      io.event();
    }
  } });
  install(t, f, pi, io, { pageSize: 7, pagesPerScan: 2 });
  await pi.emit('session_start');
  await until(() => pi.messages.length === 174, 'all pending pages and concurrent lower-ID insert');
  await pause(80);
  assert.equal(new Set(pi.messages.map(item => item.message.details.receiptId)).size, 174);
  assert.ok(inboxCalls(io).every(call => call.args.includes('--pending') && call.returnedRows <= 7));
  assert.ok(inboxCalls(io).length < 60);
  assert.equal(io.peak, 1);
  await pi.emit('session_shutdown');
  const resumed = fakePi(f.dir, { entries: structuredClone(pi.entries) });
  install(t, f, resumed, io, { pageSize: 7, pagesPerScan: 2 });
  const before = io.calls.length;
  await resumed.emit('session_start', { reason: 'resume' });
  await until(() => io.calls.length - before >= 25, 'resumed full pending sweep');
  await pause(60);
  assert.equal(resumed.messages.length, 0);
});

test('fixture: an oversized pending page shrinks its limit, never its scope or receipt cursor', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const rows = Array.from({ length: 5 }, (_, index) => receipt('session-a', {
    receipt_id: idFor(index + 1), payload: { ...receipt().payload, run_id: index + 1 },
  }));
  const io = fakeIO(rows, { response: (_verb, _recipient, call) => {
    if (Number(call.args[call.args.indexOf('--limit') + 1]) > 2) throw Object.assign(new Error('bounded stdout overflow'), { code: 'ERR_CHILD_PROCESS_STDIO_MAXBUFFER' });
  } });
  install(t, f, pi, io, { pageSize: 8 });
  await pi.emit('session_start');
  await until(() => pi.messages.length === 5, 'bounded page size reduction');
  assert.deepEqual(inboxCalls(io).slice(0, 3).map(call => call.args[call.args.indexOf('--limit') + 1]), ['8', '4', '2']);
  assert.ok(inboxCalls(io).every(call => call.args.includes('--pending')));
  assert.equal(new Set(pi.messages.map(item => item.message.details.receiptId)).size, 5);
});

test('fixture: unsupported or non-advancing bounded inbox ABI fails explicitly without full-history fallback', async t => {
  for (const mode of ['unsupported', 'non-advancing', 'foreign-recipient']) {
    await t.test(mode, async t => {
      const f = fixture(t);
      const pi = fakePi(f.dir);
      const io = fakeIO([receipt()], { response: (_verb, _recipient, call) => {
        if (mode === 'unsupported') throw Object.assign(new Error('private CLI error'), { stderr: '命令参数无效；请使用 --help 查看用法' });
        if (mode === 'foreign-recipient') return JSON.stringify([receipt('someone-else')]);
        if (call.args.includes('--after')) return JSON.stringify([receipt()]);
      } });
      install(t, f, pi, io, { pageSize: 1 });
      await pi.emit('session_start');
      assert.ok(pi.notices.some(notice => /分页|有界|新版/.test(notice[0])));
      assert.ok(inboxCalls(io).every(call => call.args.includes('--pending') && call.args.includes('--limit')));
      assert.equal(ackCalls(io).length, 0);
    });
  }
});

test('actual TestLog outcomes remain distinct in model-visible fixture data; unknown outcomes are not instructions', async t => {
  const run = promisify(nativeExecFile);
  const log = 'panic: test timed out after 14m0s\nrunning tests:\n\tTestStillRunning (17s)\n{"Action":"fail","Package":"example/store","Test":"TestAssertion"}\n';
  const parsed = await run('python3', ['-B', '-c', 'import json,sys; sys.path.insert(0,sys.argv[1]); from observer import extract_tests; print(json.dumps(extract_tests(sys.argv[2])))', here, log]);
  const tests = JSON.parse(parsed.stdout);
  assert.deepEqual(new Set(tests.map(item => item.outcome)), new Set(['failed', 'running_at_timeout']));
  tests.push({ name: 'TestUnknown', package: null, outcome: 'EXECUTE_ARBITRARY_INSTRUCTION' });
  const f = fixture(t);
  const pi = fakePi(f.dir);
  install(t, f, pi, fakeIO([receipt('session-a', { payload: { ...receipt().payload, tests } })]));
  await pi.emit('session_start');
  const data = dataFor(pi.messages[0].message);
  assert.equal(data.tests.find(item => item.name === 'TestStillRunning').outcome, 'running_at_timeout');
  assert.equal(data.tests.find(item => item.name === 'TestAssertion').outcome, 'failed');
  assert.equal(data.tests.find(item => item.name === 'TestUnknown').outcome, undefined);
  assert.ok(!pi.messages[0].message.content.includes('EXECUTE_ARBITRARY_INSTRUCTION'));
});

test('fixture: late multi-shard details stay information after a persisted or queued first wake', async t => {
  for (const persist of [true, false]) await t.test(persist ? 'persisted first wake' : 'queued first wake', async t => {
    const f = fixture(t);
    const pi = fakePi(f.dir, { persist });
    const io = fakeIO([receipt()]);
    install(t, f, pi, io, { pageSize: 5, pagesPerScan: 2 });
    await pi.emit('session_start');
    assert.equal(pi.messages.length, 1);
    for (let index = 0; index < 12; index++) io.rows.push(receipt('session-a', {
      receipt_id: idFor(index + 1), payload: { ...receipt().payload, action: 'job_details', job: String(501 + index), job_id: 501 + index },
    }));
    writeFileSync(f.db, 'old multi-shard details committed after first wake');
    io.event();
    await until(() => pi.messages.length === 13, 'first failure plus information receipts');
    assert.equal(pi.messages.filter(item => item.options.triggerTurn).length, 1);
    for (const { message, options } of pi.messages.filter(item => item.message.details.actionIdentity.action === 'job_details')) {
      assert.equal(options.triggerTurn, false);
      assert.match(message.content, /补报不计作向负责人再次派工/);
      assert.ok(!message.content.includes('ci_observer_begin_receipt'));
    }
    assert.equal(ackCalls(io).length, 0);
    if (persist) {
      await begin(pi);
      assert.equal((await waitDetails(pi, receiptId)).details.waiting, false);
    } else await assert.rejects(waitDetails(pi, receiptId), /真实首报/);
    assert.equal(pi.messages.filter(item => item.options.triggerTurn).length, 1);
  });
});

test('fixture: an explicit durable detail wait resumes only its exact existing action once', async t => {
  for (const abandoned of [false, true]) await t.test(abandoned ? 'abandoned branch' : 'active branch', async t => {
    const f = fixture(t);
    const pi = fakePi(f.dir);
    const io = fakeIO([receipt()]);
    install(t, f, pi, io);
    await pi.emit('session_start');
    await begin(pi);
    await waitDetails(pi, receiptId);
    await pi.emit('session_shutdown');
    const resumed = fakePi(f.dir, { entries: structuredClone(pi.entries) });
    install(t, f, resumed, io);
    if (abandoned) resumed.manager.getBranch = () => resumed.entries.filter(entry => entry.customType !== 'ci-observer-details-wait');
    await resumed.emit('session_start', { reason: 'resume' });
    io.rows.push(receipt('session-a', { receipt_id: idFor(2), payload: { ...receipt().payload, action: 'job_details', run_id: 92 } }));
    io.rows.push(receipt('session-a', { receipt_id: idFor(3), payload: { ...receipt().payload, action: 'job_details', job: '@target', detail_kind: 'target_binding', tested_sha: 'b'.repeat(40) } }));
    io.rows.push(receipt('session-a', { receipt_id: idFor(4), payload: { ...receipt().payload, action: 'job_details', detail_kind: 'logs' } }));
    writeFileSync(f.db, 'three detail receipts committed');
    io.event();
    await until(() => resumed.messages.length === 3, 'detail information and exact continuation');
    assert.equal(resumed.messages.find(item => item.message.details.detailKind === 'target_binding').options.triggerTurn, false);
    const wakes = resumed.messages.filter(item => item.options.triggerTurn);
    assert.equal(wakes.length, abandoned ? 0 : 1);
    if (!abandoned) {
      assert.equal(wakes[0].message.details.continuationOf, receiptId);
      assert.equal(wakes[0].message.details.actionIdentity.run_id, 91);
      assert.ok(!wakes[0].message.content.includes('ci_observer_begin_receipt'));
    }
    await resumed.emit('session_shutdown');
    const again = fakePi(f.dir, { entries: structuredClone(resumed.entries) });
    install(t, f, again, io);
    await again.emit('session_start', { reason: 'resume' });
    assert.equal(again.messages.length, 0);
  });
});

test('fixture: only the known Release Full ready action is enabled, not arbitrary future actions', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const payload = {
    ...receipt().payload, action: 'release_full_ready', workflow: '.github/workflows/release-candidate.yml',
    provenance: 'release-intent', tested_sha: 'a'.repeat(40), target_unknown: false, conclusion: null,
  };
  const candidates = [payload, ...[
    { action: 'gate_check' }, { action: 'arbitrary_target_update' }, { provenance: 'source_only' },
    { workflow: '.github/workflows/merge-gate.yml' }, { target_unknown: true },
    { tested_sha: 'b'.repeat(40) }, { conclusion: 'failure' }, { job: '@terminal' },
  ].map(patch => ({ ...payload, ...patch }))];
  const io = fakeIO(candidates.map((payload, index) => receipt('session-a', { receipt_id: idFor(index + 1), payload })));
  install(t, f, pi, io);
  await pi.emit('session_start');
  assert.equal(pi.messages.length, 1);
  assert.equal(pi.messages[0].message.details.actionIdentity.action, 'release_full_ready');
  assert.equal(pi.messages[0].options.triggerTurn, true);
});

test('fixture: core attribution cause keys accept only the exact commit/PR namespace', async t => {
  const f = fixture(t);
  const pi = fakePi(f.dir);
  const payload = { ...receipt().payload, action: 'attribution', commit: 'c'.repeat(40), pr: 22 };
  payload.job = `@attribution:${payload.commit}:${payload.pr}`;
  const io = fakeIO([
    receipt('session-a', { payload }),
    receipt('session-a', { receipt_id: secondId, payload: { ...payload, pr: 23 } }),
  ]);
  install(t, f, pi, io);
  await pi.emit('session_start');
  assert.equal(pi.messages.length, 1);
  assert.equal(pi.messages[0].message.details.actionIdentity.job, payload.job);
});

test('fixture: ambiguous begin I/O re-reads the exact receipt before at most one idempotent resubmission', async t => {
  for (const mode of ['committed-output-lost', 'committed-output-overflow', 'busy-before-commit', 'readback-unavailable', 'timeout', 'success-without-ack']) {
    await t.test(mode, async t => {
      const f = fixture(t);
      const pi = fakePi(f.dir);
      let writes = 0;
      const io = fakeIO([receipt()], { response: (verb, _recipient, call) => {
        if (verb === 'inbox' && call.args.includes('--receipt') && writes && mode === 'readback-unavailable') throw new Error('unknown outcome');
        if (verb !== 'ack') return;
        writes++;
        if (mode.startsWith('committed-')) {
          Object.assign(io.rows[0], { state: 'acked', acked_at: ackedAt });
          throw Object.assign(new Error('output lost after commit'), { code: mode.endsWith('overflow') ? 'ERR_CHILD_PROCESS_STDIO_MAXBUFFER' : 'EIO' });
        }
        if (mode === 'success-without-ack') return '';
        if (mode === 'timeout') throw Object.assign(new Error('timed out'), { killed: true });
        if (writes === 1) throw new Error('sqlite busy before commit');
      } });
      install(t, f, pi, io);
      await pi.emit('session_start');
      const firstCall = io.calls.length;
      if (['readback-unavailable', 'timeout', 'success-without-ack'].includes(mode)) await assert.rejects(begin(pi));
      else assert.equal((await begin(pi)).details.ackedAt, ackedAt);
      const commands = io.calls.slice(firstCall);
      assert.ok(commands.filter(call => call.args.includes('inbox')).every(call => call.args.includes('--receipt') && !call.args.includes('--pending')));
      const indices = commands.flatMap((call, index) => call.args.includes('ack') ? [index] : []);
      assert.ok(indices.length >= 1 && indices.length <= 2);
      if (indices.length === 2) assert.ok(commands.slice(indices[0] + 1, indices[1]).some(call => call.args.includes('--receipt') && call.args.includes('inbox')));
      assert.equal(indices.length, mode.startsWith('committed-') || mode === 'readback-unavailable' ? 1 : 2);
      assert.ok(commands.length <= 7);
      if (mode.startsWith('committed-')) assert.equal(io.rows[0].acked_at, ackedAt);
    });
  }
});

test('real producer duplicate replay stays visible information with owner proof, no begin or restart wake', async t => {
  const f = fixture(t);
  rmSync(f.db);
  const run = promisify(nativeExecFile);
  const { stdout } = await run('python3', ['-B', '-c', `
import json,sys
sys.path.insert(0,sys.argv[1])
from test_release_duplicates import Exchange,REPO
from store import Store
from github import API
from observer import Observer
s=Store(sys.argv[2]);s.configure(REPO,'session-a')
w=Exchange();o=Observer(s,API(s,w));o.pump=lambda:None
assert o.process(w.run,w.jobs,True)
print(json.dumps(s.inbox('session-a')))
s.close()
`, here, f.db], { timeout: 10000 });
  const rows = JSON.parse(stdout);
  assert.equal(rows.length, 1);
  const pi = fakePi(f.dir);
  const io = fakeIO(rows);
  install(t, f, pi, io);
  await pi.emit('session_start');
  assert.equal(pi.messages.length, 1);
  const { message, options } = pi.messages[0];
  const data = dataFor(message);
  assert.equal(options.triggerTurn, false);
  assert.equal(data.action, 'terminal');
  assert.equal(data.conclusion, 'failure');
  assert.equal(data.investigation, false);
  assert.equal(data.release_duplicate.owner_run_id, 10);
  assert.equal(data.release_duplicate.owner_url, 'https://github.com/owner/project/actions/runs/10');
  assert.equal(data.release_duplicate.provenance, 'native-release-owner-v1');
  assert.equal(data.release_duplicate.admission_job_id, 21);
  assert.equal(data.release_duplicate.status_creator_id, 41898282);
  assert.ok(!message.content.includes('ci_observer_begin_receipt'));
  await assert.rejects(begin(pi, { receiptId: rows[0].receipt_id }), /不需要另行开始处理/);
  assert.equal(ackCalls(io).length, 0);
  await pi.emit('session_shutdown');
  const resumed = fakePi(f.dir, { entries: structuredClone(pi.entries) });
  install(t, f, resumed, io);
  await resumed.emit('session_start');
  assert.equal(resumed.messages.length, 0);

  for (const change of [{ owner_url: 'https://github.com/other/project/actions/runs/10' },
    { run_id: 12 }, { candidate_tree: 'f'.repeat(40) }, { provenance: 'unverified' }, { status_creator_id: 7 }, { owner_status: {} }]) {
    const forged = structuredClone(rows[0]);
    Object.assign(forged.payload.release_duplicate, change);
    const other = fakePi(f.dir);
    const badIO = fakeIO([forged]);
    install(t, f, other, badIO);
    await other.emit('session_start');
    assert.equal(other.messages.length, 1, 'a fake duplicate cannot hide an ordinary terminal');
    assert.equal(other.messages[0].options.triggerTurn, true);
    assert.ok(other.messages[0].message.content.includes('ci_observer_begin_receipt'));
    assert.equal(dataFor(other.messages[0].message).release_duplicate, undefined);
    await other.emit('session_shutdown');
  }
});

test('real offline Python ABI/OS watcher with fixture Pi API: workflow/tag delivery, ack, restart and quiescence', async t => {
  const f = fixture(t);
  rmSync(f.db);
  const run = promisify(nativeExecFile);
  const seed = async (code, timeoutMs = 10000) => run('python3', ['-B', '-c', `import sys\nsys.path.insert(0,sys.argv[1])\nfrom store import Store, Key\ns=Store(sys.argv[2])\n${code}\ns.close()`, here, f.db], { timeout: timeoutMs });
  // The 10 s seed watchdog expired under load before receiver startup; only history setup gets 60 s.
  const { stdout: historyOutput } = await seed(`
s.configure('example/project','session-a')
import json
from store import now
with s.db:
    # Include the WITH-prefixed INSERT in the same transaction as both dependent tables.
    s.db.execute('BEGIN IMMEDIATE')
    s.db.execute("""
        WITH RECURSIVE history(run_id) AS (
            VALUES(1000) UNION ALL SELECT run_id+1 FROM history WHERE run_id<5999
        )
        INSERT INTO actions(id,repo,sha,run_id,attempt,job,kind,payload,created_at)
        SELECT printf('%064x',run_id),:repo,:sha,run_id,1,'@terminal','terminal',
               json_object('repo',:repo,'sha',:sha,'run_id',run_id,'attempt',1,
                           'action','terminal','job','@terminal','body',:body),:stamp
        FROM history
    """, dict(repo='example/project',sha='${'a'.repeat(40)}',body='x'*1200,stamp=now()))
    s.db.execute("""
        INSERT INTO outbox(action_id,recipient,receipt_id,state,delivered_at,acked_at)
        SELECT id,'session-a',id,'acked',created_at,created_at FROM actions
    """)
    s.db.execute("""
        INSERT INTO inbox(receipt_id,recipient,payload,delivered_at,acked_at)
        SELECT o.receipt_id,o.recipient,a.payload,o.delivered_at,o.acked_at
        FROM outbox o JOIN actions a ON a.id=o.action_id
    """)
acked=s.db.execute("""
    SELECT count(*) FROM actions a JOIN outbox o ON o.action_id=a.id
    JOIN inbox i ON i.receipt_id=o.receipt_id
    WHERE o.state='acked' AND o.acked_at IS NOT NULL AND i.acked_at=o.acked_at
          AND i.delivered_at=o.delivered_at AND i.recipient=o.recipient AND o.recipient='session-a'
""").fetchone()[0]
print(json.dumps(dict(
    counts={table:s.db.execute('SELECT count(*) FROM '+table).fetchone()[0] for table in ('actions','outbox','inbox')},
    acked=acked,
    payload_bytes=s.db.execute('SELECT sum(length(CAST(payload AS BLOB))) FROM inbox').fetchone()[0],
    foreign_key_violations=len(s.db.execute('PRAGMA foreign_key_check').fetchall()),
)))`, 60000);
  const history = JSON.parse(historyOutput);
  assert.deepEqual(history.counts, { actions: 5000, outbox: 5000, inbox: 5000 });
  assert.equal(history.acked, 5000);
  assert.equal(history.foreign_key_violations, 0);
  assert.ok(history.payload_bytes > 4 * 1024 * 1024, 'acknowledged history exceeds the receiver output cap');
  t.diagnostic(`Verified SQLite ACK history ${JSON.stringify(history)}`);
  const pi = fakePi(f.dir);
  let reads = 0;
  let running = 0;
  const commands = [];
  installReceiver(pi.pi, {
    env: { CI_OBSERVER_DB: f.db }, debounceMs: 30,
    execFile(command, args, options, callback) {
      commands.push(args);
      if (args.includes('inbox')) reads++;
      running++;
      return nativeExecFile(command, args, options, (...result) => { running--; callback(...result); });
    },
  });
  t.after(() => pi.emit('session_shutdown'));
  await pi.emit('session_start');
  assert.deepEqual(pi.notices, [], 'real CLI must implement the bounded inbox ABI');
  await seed(`s.publish(Key('example/project','${'a'.repeat(40)}',91,2,'501'),'first_failure',{'job_id':501,'tests':[]},['session-a'])`);
  await pause(160);
  assert.equal(pi.messages.length, 0);
  await seed(`s.deliver(('example/project','${'a'.repeat(40)}',91,2),'session-a')`);
  await until(() => pi.messages.length === 1, 'real OS watcher delivery', 5000);
  const actualId = pi.messages[0].message.details.receiptId;
  const result = await begin(pi, { receiptId: actualId });
  assert.ok(Date.parse(result.details.ackedAt));
  const again = await begin(pi, { receiptId: actualId });
  assert.equal(again.details.ackedAt, result.details.ackedAt);
  const tagPayload = tagReceipt().payload;
  await seed(`import json\nfrom rc_refs import source_delivery\nkey=Key('example/project','${tagPayload.sha}',None,None,${JSON.stringify(tagPayload.job)})\ns.publish(key,'release_tag_forward',json.loads(${JSON.stringify(JSON.stringify(tagPayload))}),['session-a'])\nsource_delivery(s,key,'session-a',True)`);
  await until(() => pi.messages.length === 2, 'real CLI tag delivery', 5000);
  const tagId = pi.messages[1].message.details.receiptId;
  const tagResult = await begin(pi, { receiptId: tagId });
  assert.ok(Date.parse(tagResult.details.ackedAt));
  assert.equal(tagResult.details.actionIdentity.tag, tagPayload.tag);
  assert.equal(tagResult.details.toolCallId, 'call_fixture|fc_fixture');
  await pause(250);
  await until(() => running === 0, 'real CLI commands settled before quiescence check', 5000);
  const settledReads = reads;
  await pause(300);
  assert.equal(reads, settledReads, 'CLI inbox reads must not recursively trigger themselves');
  assert.ok(commands.filter(args => args.includes('inbox')).every(args => args.includes('--pending') || args.includes('--receipt')));
  await pi.emit('session_shutdown');
  const resumed = fakePi(f.dir, { entries: structuredClone(pi.entries) });
  installReceiver(resumed.pi, { env: { CI_OBSERVER_DB: f.db } });
  t.after(() => resumed.emit('session_shutdown'));
  await resumed.emit('session_start');
  assert.equal(resumed.messages.length, 0);
});
