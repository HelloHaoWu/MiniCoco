import {test} from 'node:test';
import assert from 'node:assert/strict';
import * as http from 'node:http';
import * as fs from 'node:fs';
import * as path from 'node:path';
import * as os from 'node:os';
import {once} from 'node:events';
import {consumeEvents, runRound, runRoundNonStreaming, RoundError, type RoundResult} from './executor.ts';
import {runRoundWithRetry} from './retry.ts';
import {RETRY_POLICY} from './tuning.ts';
import {Conversation} from './conversation.ts';
import type {Event, Message} from './protocol.ts';

const args = {serverUrl: 'http://unused.invalid', sessionId: '', roundId: 'round-test', query: 'ask', workspace: '/tmp', model: ''};
const done: Event = {type: 'done', finish_reason: 'stop'};
const successful: Event[] = [{type: 'reasoning', delta: 'thinking'}, {type: 'text', delta: '中文'}, done];
const result: RoundResult = {assistant: {role: 'assistant', content: 'ok'}, toolCalls: [], finish: 'stop'};

async function server(handler: http.RequestListener, run: (url: string) => Promise<void>): Promise<void> {
    const instance = http.createServer((req, res) => {if (req.url === '/audit') {res.writeHead(202);res.end('{}');return;}res.setHeader('X-Session-Id', 's_test');handler(req, res);});instance.listen(0, '127.0.0.1');await once(instance, 'listening');
    try {await run(`http://127.0.0.1:${(instance.address() as import('node:net').AddressInfo).port}`);}
    finally {instance.closeAllConnections();await new Promise<void>(resolve => instance.close(() => resolve()));}
}

test('SSE and non-stream JSON use the same pure event consumer and produce equal results', async () => {
    const requests: boolean[] = [];
    await server(async (req, res) => {
        let body = '';for await (const chunk of req) body += chunk;
        const {stream} = JSON.parse(body) as {stream: boolean};requests.push(stream);
        res.setHeader('Content-Type', stream ? 'text/event-stream' : 'application/json');
        res.end(stream ? successful.map(event => `data: ${JSON.stringify(event)}\n\n`).join('') : JSON.stringify({events: successful}));
    }, async url => {
        const a = await runRound({...args, serverUrl: url, options: {onEvent: () => {}}});
        const b = await runRoundNonStreaming({...args, serverUrl: url, options: {onEvent: () => {}}});
        assert.deepEqual(a, b);assert.equal(b.assistant.content, '中文');assert.deepEqual(requests, [true, false]);
    });
});

test('non-stream JSON rejects malformed, oversize and invalid envelopes with typed errors', async () => {
    for (const [body, contentType, kind] of [
        ['invalid', 'application/json', 'protocol'], ['{}', 'application/json', 'protocol'],
        [Buffer.from([0xff]), 'application/json', 'protocol'],
        ['{"events":null}', 'application/json', 'protocol'], ['{"events":[]}', 'application/json', 'network'],
        [JSON.stringify({events: [{type: 'unknown'}]}), 'application/json', 'protocol'],
        [JSON.stringify({events: [{type: 'text', delta: 'x'.repeat(1024 * 1024)}, done]}), 'application/json', 'protocol'],
        [JSON.stringify({events: [done, {type: 'text', delta: 'after done'}]}), 'application/json', 'protocol'],
        [JSON.stringify({events: [{type: 'error', message: 'quota', code: 'quota_exhausted', retryable: false}, done]}), 'application/json', 'backend'],
        ['x'.repeat(4 * 1024 * 1024 + 1), 'application/json', 'protocol'],
        [JSON.stringify({events: successful}), 'text/plain', 'protocol'],
    ] as const) await server((req, res) => {res.setHeader('Content-Type', contentType);res.end(body);}, async url => {
        await assert.rejects(() => runRoundNonStreaming({...args, serverUrl: url}), (error: unknown) => error instanceof RoundError && error.kind === kind);
    });
});

test('both event forms reject duplicate, excessive or inconsistent tool calls', async () => {
    const call: Event = {type: 'tool_call', id: 'id', name: 'list_dir', arguments: {}};
    for (const events of [
        [call, call, {type: 'done', finish_reason: 'tool_calls'}],
        [call, done],
        [{type: 'done', finish_reason: 'tool_calls'}],
        [...Array.from({length: 17}, (_, i) => ({...call, id: String(i)})), {type: 'done', finish_reason: 'tool_calls'}],
    ] as Event[][]) {
        await assert.rejects(() => consumeEvents(events), RoundError);
        await assert.rejects(() => consumeEvents((async function* () {yield* events;})()), RoundError);
    }
});

test('stream exhaustion triggers exactly one fallback: seven explicit attempts or eight by default', async () => {
    for (const maxRetries of [6, RETRY_POLICY.maxRetries]) {
        let streams = 0, fallbacks = 0;
        const recovered = await runRoundWithRetry(args, {...RETRY_POLICY, maxRetries}, undefined, {
            run: async () => {streams++;throw new RoundError('transient', 'network');},
            runNonStreaming: async () => {fallbacks++;return result;}, sleep: async () => {}, onRetry: () => {},
        });
        assert.equal(streams, maxRetries + 1);assert.equal(fallbacks, 1);assert.equal(recovered.attempts, maxRetries + 2);
    }
});

test('fallback failure is final even when transient; fatal, cancellation and compression failure never fallback', async () => {
    let fallbacks = 0;
    const dependencies = {run: async (): Promise<RoundResult> => {throw new RoundError('transient', 'network');}, runNonStreaming: async (): Promise<RoundResult> => {fallbacks++;throw new RoundError('still transient', 'network');}, sleep: async () => {}, onRetry: () => {}};
    await assert.rejects(() => runRoundWithRetry(args, {...RETRY_POLICY, maxRetries: 0}, undefined, dependencies), (error: unknown) => error instanceof RoundError && error.attempts === 2);
    assert.equal(fallbacks, 1);
    for (const error of [new RoundError('fatal', 'http', {httpStatus: 400}), new RoundError('context', 'backend', {code: 'need_compression'}), new RoundError('protocol', 'protocol')]) {
        await assert.rejects(() => runRoundWithRetry(args, {...RETRY_POLICY, maxRetries: 0}, undefined, {...dependencies, run: async () => {throw error;}}), RoundError);
    }
    const controller = new AbortController();controller.abort();
    await assert.rejects(() => runRoundWithRetry(args, RETRY_POLICY, controller.signal, dependencies), RoundError);
    assert.equal(fallbacks, 1);
});

test('fallback uses exactly the same round identity and delta as failed streaming attempts', async () => {
    const received: unknown[] = [];
    const recovered = await runRoundWithRetry(args, {...RETRY_POLICY, maxRetries: 0}, undefined, {
        run: async current => {received.push({...current, options: undefined});throw new RoundError('network', 'network');},
        runNonStreaming: async current => {received.push({...current, options: undefined});return result;},
    });
    assert.equal(recovered.attempts, 2);assert.deepEqual(received[0], received[1]);assert(!('messages' in (received[0] as object)));
});

test('non-stream request and body waits respect timeout and user cancellation', async () => {
    await server((req, res) => {res.writeHead(200, {'Content-Type': 'application/json'});res.flushHeaders();}, async url => {
        await assert.rejects(() => runRoundNonStreaming({...args, serverUrl: url, options: {timeoutMs: 20}}), (error: unknown) => error instanceof RoundError && error.kind === 'timeout');
        const controller = new AbortController();setTimeout(() => controller.abort(), 10);
        await assert.rejects(() => runRoundNonStreaming({...args, serverUrl: url}, controller.signal), (error: unknown) => error instanceof RoundError && error.kind === 'abort');
    });
});

test('a real fallback response runs tools once and the next round independently retries', async () => {
    const ws = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-fallback-'));
    const transports: boolean[] = [];
    try {
        await server(async (req, res) => {
            let body = '';for await (const chunk of req) body += chunk;
            const input = JSON.parse(body) as {stream: boolean; toolUseResults?: unknown[]};transports.push(input.stream);
            if (input.stream) {res.writeHead(503);res.end();return;}
            const events: Event[] = input.toolUseResults?.length ? [{type: 'text', delta: 'done'}, done] : [
                {type: 'tool_call', id: 'once', name: 'run_command', arguments: {command: 'printf x >> count'}}, {type: 'done', finish_reason: 'tool_calls'},
            ];
            res.setHeader('Content-Type', 'application/json');res.end(JSON.stringify({events}));
        }, async url => {
            const convo = new Conversation(url, ws, '', 25, {approvalMode: 'yes', retryPolicy: {...RETRY_POLICY, maxRetries: 1}, retryDependencies: {sleep: async () => {}, onRetry: () => {}}});
            const outcome = await convo.run('run once');assert.equal(outcome.status, 'completed', outcome.message);
            assert.equal(fs.readFileSync(path.join(ws, 'count'), 'utf8'), 'x');assert(!('messages' in convo));
            assert.deepEqual(transports, [true, true, false, true, true, false]);
        });
    } finally {fs.rmSync(ws, {recursive: true, force: true});}
});
