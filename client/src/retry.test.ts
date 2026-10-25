import {test} from 'node:test';
import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import * as http from 'node:http';
import {getEventListeners, once} from 'node:events';
import {runRound, RoundError, type RoundResult} from './executor.ts';
import {runRoundWithRetry, backoffDelay, backoffSleep, classify, type CompressionHook} from './retry.ts';
import {Conversation} from './conversation.ts';
import {RETRY_POLICY, type RetryPolicy} from './tuning.ts';
import type {Event, Message} from './protocol.ts';

const fast: RetryPolicy = {...RETRY_POLICY, baseMs: 0, capMs: 0, jitter: 0, maxRetries: 2};
const success: RoundResult = {assistant: {role: 'assistant', content: 'ok'}, toolCalls: [], finish: 'stop'};
const args = {serverUrl: 'http://127.0.0.1:8017', messages: [{role: 'user', content: 'ask'}] as Message[], workspace: '/tmp', model: ''};
const frame = (...events: Event[]): string => events.map(event => `data: ${JSON.stringify(event)}\n\n`).join('');
const stop: Event = {type: 'done', finish_reason: 'stop'};

async function withServer(handler: http.RequestListener, run: (url: string) => Promise<void>): Promise<void> {
    const server = http.createServer(handler);server.listen(0, '127.0.0.1');await once(server, 'listening');
    try {await run(`http://127.0.0.1:${(server.address() as import('node:net').AddressInfo).port}`);}
    finally {server.closeAllConnections();await new Promise<void>(resolve => server.close(() => resolve()));}
}

test('round errors classify transient/fatal/compress conservatively, with cancellation taking precedence', () => {
    for (const [error, expected] of [
        [new RoundError('network', 'network'), 'transient'],
        [new RoundError('timeout', 'timeout'), 'transient'],
        [new RoundError('abort', 'abort'), 'fatal'],
        [new RoundError('protocol', 'protocol'), 'fatal'],
        [new RoundError('legacy server', 'backend'), 'fatal'],
        [new RoundError('quota', 'backend', {code: 'quota_exhausted', retryable: false}), 'fatal'],
        [new RoundError('retry', 'backend', {retryable: true}), 'transient'],
        [new RoundError('compress', 'backend', {code: 'need_compression', retryable: true}), 'compress'],
        ...[400, 401, 403, 404, 413, 422].map(status => [new RoundError('http', 'http', {httpStatus: status}), 'fatal'] as const),
        ...[408, 425, 429, 500, 502, 503, 504].map(status => [new RoundError('http', 'http', {httpStatus: status}), 'transient'] as const),
    ] as const) {
        assert.equal(classify(error), expected);assert.equal(classify(error, true), 'fatal');
    }
});

test('backoff follows capped exponential delay and removes cancellation listeners on all exits', async () => {
    assert.deepEqual(Array.from({length: 7}, (_, index) => backoffDelay(index, RETRY_POLICY, () => 0)), [3000, 6000, 12000, 24000, 30000, 30000, 30000]);
    assert.equal(backoffDelay(6, RETRY_POLICY, () => 1), 37500);
    const controller = new AbortController();
    await backoffSleep(0, fast, controller.signal);
    assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
    const sleeping = backoffSleep(0, RETRY_POLICY, controller.signal);
    controller.abort();
    await assert.rejects(() => sleeping, (error: unknown) => error instanceof RoundError && error.kind === 'abort');
    assert.equal(getEventListeners(controller.signal, 'abort').length, 0);
    await assert.rejects(() => backoffSleep(0, RETRY_POLICY, controller.signal), RoundError);
});

test('two transient failures use three attempts and exactly two backoffs; success is never replayed', async () => {
    let attempts = 0;const sleeps: number[] = [];
    const result = await runRoundWithRetry(args, fast, undefined, undefined, {
        run: async () => {if (++attempts < 3) throw new RoundError('unavailable', 'http', {httpStatus: 503});return success;},
        sleep: async index => {sleeps.push(index);}, onRetry: () => {},
    });
    assert.equal(result.attempts, 3);assert.equal(attempts, 3);assert.deepEqual(sleeps, [0, 1]);
    attempts = 0;
    await runRoundWithRetry(args, fast, undefined, undefined, {run: async () => {attempts++;return success;}, sleep: async () => assert.fail('successful round backed off')});
    assert.equal(attempts, 1);
});

test('actual HTTP 503 failures replay the same request twice before recovery', async () => {
    const requests: string[] = [], sleeps: number[] = [];
    await withServer(async (req, res) => {
        let body = '';for await (const chunk of req) body += chunk;requests.push(body);
        if (requests.length < 3) {res.writeHead(503);res.end();return;}
        res.setHeader('Content-Type', 'text/event-stream');res.end(frame({type: 'text', delta: 'recovered'}, stop));
    }, async url => {
        const result = await runRoundWithRetry({...args, serverUrl: url}, fast, undefined, undefined, {sleep: async index => {sleeps.push(index);}, onRetry: () => {}});
        assert.equal(result.attempts, 3);assert.equal(result.assistant.content, 'recovered');assert.deepEqual(sleeps, [0, 1]);
        assert.equal(new Set(requests).size, 1);
    });
});

test('fatal errors and exhausted retry budgets stop at their exact limits', async () => {
    for (const failure of [new RoundError('bad input', 'http', {httpStatus: 400}), new RoundError('protocol', 'protocol'), new RoundError('legacy backend', 'backend')]) {
        let attempts = 0;
        await assert.rejects(() => runRoundWithRetry(args, fast, undefined, undefined, {run: async () => {attempts++;throw failure;}, sleep: async () => assert.fail('fatal backed off')}), (error: unknown) => error instanceof RoundError && error.attempts === 1);
        assert.equal(attempts, 1);
    }
    let attempts = 0;
    await assert.rejects(() => runRoundWithRetry(args, fast, undefined, undefined, {run: async () => {attempts++;throw new RoundError('network', 'network');}, sleep: async () => {}, onRetry: () => {}}), (error: unknown) => error instanceof RoundError && error.attempts === 3);
    assert.equal(attempts, 3);
});

test('abort during retry backoff immediately ends the round without another request', async () => {
    const controller = new AbortController();let attempts = 0;
    await assert.rejects(() => runRoundWithRetry(args, RETRY_POLICY, controller.signal, undefined, {
        run: async () => {attempts++;throw new RoundError('network', 'network');},
        onRetry: () => {setTimeout(() => controller.abort(), 10);},
    }), (error: unknown) => error instanceof RoundError && error.kind === 'abort' && error.attempts === 1);
    assert.equal(attempts, 1);
});

test('compression uses a private snapshot and can reduce bytes without reducing message count', async () => {
    const input = {...args, messages: [{role: 'user', content: 'x'.repeat(200)}, ...args.messages]};
    const original = structuredClone(input.messages);let attempts = 0, compressions = 0;
    const hook: CompressionHook = {async compress(messages, reason) {compressions++;assert.equal(reason, 'upstream');messages[0].content = 'summary';return messages;}};
    const result = await runRoundWithRetry(input, fast, undefined, hook, {
        run: async current => {if (++attempts === 1) throw new RoundError('context', 'backend', {code: 'need_compression', retryable: true});assert.equal(current.messages[0].content, 'summary');return success;},
        sleep: async () => assert.fail('compression used transient backoff'),
    });
    assert.equal(attempts, 2);assert.equal(compressions, 1);assert.deepEqual(input.messages, original);assert.equal(result.messages[0].content, 'summary');
});

test('missing, nonshrinking and structurally invalid compression hooks fail without mutating history', async () => {
    const input = {...args, messages: [{role: 'user', content: 'old '.repeat(100)}, ...args.messages]};
    const original = structuredClone(input.messages);
    const fail = async (): Promise<RoundResult> => {throw new RoundError('context', 'backend', {code: 'need_compression', retryable: true});};
    for (const hook of [undefined, {compress: async (messages: Message[]) => messages}, {compress: async () => [{role: 'tool', tool_call_id: 'orphan', content: 'x'}, ...args.messages]}, {compress: async () => [{role: 'user', content: 'different input'}]}]) {
        await assert.rejects(() => runRoundWithRetry(input, fast, undefined, hook, {run: fail}), RoundError);
        assert.deepEqual(input.messages, original);
    }
});

test('compression has an independent bounded count and can itself be cancelled', async () => {
    const input = {...args, messages: [{role: 'user', content: 'x'.repeat(1000)}, ...args.messages]};
    let requests = 0, hooks = 0;
    await assert.rejects(() => runRoundWithRetry(input, fast, undefined, {async compress(messages) {hooks++;messages[0].content = messages[0].content!.slice(0, -100);return messages;}}, {
        run: async () => {requests++;throw new RoundError('context', 'backend', {code: 'need_compression'});},
    }), RoundError);
    assert.equal(hooks, 3);assert.equal(requests, 4);
    const controller = new AbortController();
    const pending = runRoundWithRetry(input, fast, controller.signal, {compress: () => new Promise(() => {})}, {run: async () => {throw new RoundError('context', 'backend', {code: 'need_compression'});}});
    setTimeout(() => controller.abort(), 10);
    await assert.rejects(() => pending, (error: unknown) => error instanceof RoundError && error.kind === 'abort');
});

test('HTTP/SSE/network failures are typed; user cancellation remains distinct from timeout', async () => {
    for (const [status, body, type, expected] of [
        [503, '', 'text/plain', 'http'],
        [200, 'data: nope\n\n', 'text/event-stream', 'protocol'],
        [200, frame({type: 'text', delta: 'partial'}), 'text/event-stream', 'network'],
        [200, frame({type: 'error', message: 'quota', code: 'quota_exhausted', retryable: false}), 'text/event-stream', 'backend'],
    ] as const) await withServer((req, res) => {res.writeHead(status, {'Content-Type': type});res.end(body);}, async url => {
        await assert.rejects(() => runRound(url, args.messages, '/tmp', ''), (error: unknown) => error instanceof RoundError && error.kind === expected);
    });
    await withServer((req, res) => {res.writeHead(200, {'Content-Type': 'text/event-stream'});res.flushHeaders();}, async url => {
        await assert.rejects(() => runRound(url, args.messages, '/tmp', '', undefined, {timeoutMs: 20}), (error: unknown) => error instanceof RoundError && error.kind === 'timeout');
        const controller = new AbortController();setTimeout(() => controller.abort(), 10);
        await assert.rejects(() => runRound(url, args.messages, '/tmp', '', controller.signal), (error: unknown) => error instanceof RoundError && error.kind === 'abort');
    });
});

test('conversation adopts compressed history only after success and rolls back an unsuccessful input', async () => {
    const ws = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-compress-'));
    try {
        let attempts = 0;
        const convo = new Conversation(args.serverUrl, ws, '', 25, {retryPolicy: fast, compressionHook: {async compress(messages) {messages[0].content = 'summary';return messages;}}, retryDependencies: {
            run: async () => {if (++attempts === 1) throw new RoundError('context', 'backend', {code: 'need_compression'});return success;},
        }});
        convo.messages = [{role: 'user', content: 'old '.repeat(100)}];
        assert.equal((await convo.run('ask')).status, 'completed');assert.equal(convo.messages[0].content, 'summary');assert.equal(convo.messages.at(-1)?.content, 'ok');
        const before = structuredClone(convo.messages);
        const failed = new Conversation(args.serverUrl, ws, '', 25, {retryPolicy: fast, retryDependencies: {run: async () => {throw new RoundError('bad', 'http', {httpStatus: 400});}}});
        failed.messages = structuredClone(before);
        const result = await failed.run('failed input');assert.equal(result.status, 'failed');assert.match(result.message!, /after 1 tries/);assert.deepEqual(failed.messages, before);
        const controller = new AbortController();
        const cancelled = new Conversation(args.serverUrl, ws, '', 25, {retryDependencies: {run: async () => {throw new RoundError('network', 'network');}, onRetry: () => controller.abort()}});
        assert.equal((await cancelled.run('ask', controller.signal)).status, 'cancelled');assert.deepEqual(cancelled.messages, []);
    } finally {fs.rmSync(ws, {recursive: true, force: true});}
});

test('retrying an incomplete tool round never replays a local command', async () => {
    const ws = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-retry-tool-'));let requests = 0;
    try {
        await withServer(async (req, res) => {
            let raw = '';for await (const chunk of req) raw += chunk;
            const input = JSON.parse(raw) as {messages: Message[]};requests++;
            res.setHeader('Content-Type', 'text/event-stream');
            if (input.messages.at(-1)?.role === 'tool') {res.end(frame({type: 'text', delta: 'done'}, stop));return;}
            const call: Event = {type: 'tool_call', id: 'once', name: 'run_command', arguments: {command: 'printf x >> count'}};
            res.end(requests === 1 ? frame(call) : frame(call, {type: 'done', finish_reason: 'tool_calls'}));
        }, async url => {
            const convo = new Conversation(url, ws, '', 25, {retryPolicy: fast, retryDependencies: {sleep: async () => {}, onRetry: () => {}}});
            assert.equal((await convo.run('run once')).status, 'completed');
            assert.equal(fs.readFileSync(path.join(ws, 'count'), 'utf8'), 'x');
            assert.equal(convo.messages.filter(message => message.role === 'tool').length, 1);assert.equal(requests, 3);
        });
    } finally {fs.rmSync(ws, {recursive: true, force: true});}
});
