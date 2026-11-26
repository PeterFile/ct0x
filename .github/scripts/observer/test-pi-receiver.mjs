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
        callback(error, stdout ?? '', 'secret stderr must never be forwarded');
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
            finish(null, JSON.stringify(io.rows));
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
  assert.deepEqual(call.args.slice(2), ['--db', f.db, 'inbox', '--recipient', 'session-a']);
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

test('real offline Python ABI/OS watcher with fixture Pi API: workflow/tag delivery, ack, restart and quiescence', async t => {
  const f = fixture(t);
  rmSync(f.db);
  const run = promisify(nativeExecFile);
  const seed = async code => run('python3', ['-B', '-c', `import sys\nsys.path.insert(0,sys.argv[1])\nfrom store import Store, Key\ns=Store(sys.argv[2])\n${code}\ns.close()`, here, f.db], { timeout: 10000 });
  await seed("s.configure('example/project','session-a')");
  const pi = fakePi(f.dir);
  let reads = 0;
  installReceiver(pi.pi, {
    env: { CI_OBSERVER_DB: f.db }, debounceMs: 30,
    execFile(...args) { if (args[1].includes('inbox')) reads++; return nativeExecFile(...args); },
  });
  t.after(() => pi.emit('session_shutdown'));
  await pi.emit('session_start');
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
  const settledReads = reads;
  await pause(300);
  assert.equal(reads, settledReads, 'CLI inbox reads must not recursively trigger themselves');
  await pi.emit('session_shutdown');
  const resumed = fakePi(f.dir, { entries: structuredClone(pi.entries) });
  installReceiver(resumed.pi, { env: { CI_OBSERVER_DB: f.db } });
  t.after(() => resumed.emit('session_shutdown'));
  await resumed.emit('session_start');
  assert.equal(resumed.messages.length, 0);
});
