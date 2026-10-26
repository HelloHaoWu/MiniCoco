import {test} from 'node:test';
import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import * as http from 'node:http';
import {once} from 'node:events';
import {spawn, spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {Conversation} from './conversation.ts';
import {runRound, RoundError, type RoundResult} from './executor.ts';
import {listSessions, getSession} from './sessions.ts';
import {RETRY_POLICY} from './tuning.ts';
import type {Message, RoundRequest} from './protocol.ts';

const project = fileURLToPath(new URL('../..', import.meta.url));
const fast = {...RETRY_POLICY, maxRetries: 1, baseMs: 0, capMs: 0, jitter: 0};
const done: RoundResult = {sessionId: 's_test', assistant: {role: 'assistant', content: 'ok'}, toolCalls: [], finish: 'stop'};

async function server(handler: http.RequestListener, fn: (url: string) => Promise<void>): Promise<void> {
    const instance = http.createServer((req, res) => {if(req.url === '/audit') {res.writeHead(202);res.end('{}');return;}handler(req,res);});instance.listen(0, '127.0.0.1');await once(instance, 'listening');
    try {await fn(`http://127.0.0.1:${(instance.address() as import('node:net').AddressInfo).port}`);}
    finally {instance.closeAllConnections();await new Promise<void>(resolve => instance.close(() => resolve()));}
}

async function cli(args: string[]): Promise<{code: number | null; stdout: string; stderr: string}> {
    const child = spawn(path.join(project, 'run_client.sh'), args, {stdio: ['pipe', 'pipe', 'pipe']});
    let stdout = '', stderr = '';
    child.stdout.on('data', data => {stdout += data;});child.stderr.on('data', data => {stderr += data;});
    const exit = once(child, 'close');child.stdin.end();
    const [code] = await exit;
    return {code, stdout, stderr};
}

async function backend(binary: string, temp: string, upstream: string): Promise<{url: string; stop: () => Promise<void>}> {
    const reservation = http.createServer();reservation.listen(0, '127.0.0.1');await once(reservation, 'listening');
    const port = (reservation.address() as import('node:net').AddressInfo).port;
    await new Promise<void>(resolve => reservation.close(() => resolve()));
    const child = spawn(binary, [], {cwd: temp, env: {...process.env, STORE_BACKEND: 'file', MINICOCO_MODEL_IS_FUNCTION_CALL: '', MINICOCO_MODELS_FILE: '', MINICOCO_DEBUG_PROMPT: '0', MINICOCO_DATA_DIR: path.join(temp, 'data'), MINICOCO_HOST: '127.0.0.1', MINICOCO_PORT: String(port),
        LLM_PROVIDER: 'openai', LLM_BASE_URL: upstream, LLM_MODEL: 'deepseek-flash', LLM_API_KEY: 'fake-test-key'}, stdio: 'ignore'});
    const exit = once(child, 'exit');
    const stop = async (): Promise<void> => {if (child.exitCode === null) child.kill('SIGTERM');await exit;};
    for (let i = 0; i < 100; i++) {
        try {if ((await fetch(`http://127.0.0.1:${port}/health`)).ok) return {url: `http://127.0.0.1:${port}`, stop};} catch {}
        if (child.exitCode !== null) break;
        await new Promise(resolve => setTimeout(resolve, 20));
    }
    await stop();throw new Error('backend did not start');
}

function build(temp: string): string {
    const binary = path.join(temp, 'backend');
    const built = spawnSync('go', ['build', '-o', binary, '.'], {cwd: path.join(project, 'backend'), encoding: 'utf8'});
    assert.equal(built.status, 0, built.stderr);
    return binary;
}

function upstreamResponse(res: http.ServerResponse, delta: unknown, finish: string): void {
    res.setHeader('Content-Type', 'text/event-stream');
    res.end(`data: ${JSON.stringify({choices: [{delta, finish_reason: finish}]})}\n\ndata: ${JSON.stringify({choices: [], usage: {prompt_tokens: 3, completion_tokens: 2, total_tokens: 5}})}\n\ndata: [DONE]\n\n`);
}

test('real CLI --resume/--list survive backend restart and retrieve history from disk', {timeout: 30_000}, async () => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-resume-'));
    const ws = path.join(temp, 'workspace');fs.mkdirSync(ws);
    const received: Message[][] = [];
    try {
        const binary = build(temp);
        await server(async (req, res) => {
            let raw = '';for await (const chunk of req) raw += chunk;
            const input = JSON.parse(raw) as {messages: Message[]};received.push(input.messages);
            upstreamResponse(res, {content: input.messages.filter(message => message.role === 'user').map(message => message.content).join('|')}, 'stop');
        }, async upstream => {
            let running = await backend(binary, temp, upstream);
            try {
                const first = await cli(['remember-key', '--workspace', ws, '--server', running.url]);
                assert.equal(first.code, 0, first.stderr);
                const sid = first.stderr.match(/\[session\] (s_[a-f0-9]+)/)?.[1];assert(sid, first.stderr);
                const listed = await cli(['--list', '--server', running.url]);
                assert.equal(listed.code, 0, listed.stderr);assert.match(listed.stdout, new RegExp(sid));
                assert.equal((await listSessions(running.url)).length, 1);
                const info = await getSession(running.url, sid);
                assert.equal(info.session.total_tokens, 5);assert.equal(info.session.message_count, 2);assert.equal(info.canResume, true);
                const log = path.join(temp, 'data/transcripts', sid + '.jsonl');
                const initialRows = fs.readFileSync(log, 'utf8').trim().split('\n').map(line => JSON.parse(line));
                assert.deepEqual(initialRows.map(row => row.status), ['final', 'pending', 'final']);
                await running.stop();
                running = await backend(binary, temp, upstream);
                const resumed = await cli(['next-query', '--resume', sid, '--server', running.url]);
                assert.equal(resumed.code, 0, resumed.stderr);assert.match(resumed.stdout, /remember-key\|next-query/);
                assert.equal(received.length, 2);
                assert.deepEqual(received[1].map(message => message.role), ['system', 'user', 'user', 'assistant', 'user']);
                assert(received[1].every(message => !('msg_id' in message) && !('status' in message)));
                const updated = await getSession(running.url, sid);
                assert.equal(updated.session.total_tokens, 10);assert.equal(updated.session.message_count, 4);
                assert.equal((await listSessions(running.url)).length, 1);
                assert.equal(fs.readFileSync(log, 'utf8').trim().split('\n').length, 6);
                assert.equal((await cli(['ask', '--resume', 's_missing', '--server', running.url])).code, 1);
                assert.equal((await cli(['ask', '--resume', sid, '--workspace', temp, '--server', running.url])).code, 1);
                assert.equal((await cli(['ask', '--list', '--server', running.url])).code, 1);
                assert.equal((await cli(['--resume'])).code, 1);
                assert.equal(received.length, 2, 'invalid resume must not call a model');
            } finally {await running.stop();}
        });
    } finally {fs.rmSync(temp, {recursive: true, force: true});}
});

test('lost first response/body replay durable rounds without creating sessions or rerunning tools', {timeout: 30_000}, async () => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-replay-'));
    let modelCalls = 0;
    try {
        const binary = build(temp);
        await server(async (req, res) => {
            let raw = '';for await (const chunk of req) raw += chunk;
            const input = JSON.parse(raw) as {messages: Message[]};modelCalls++;
            if (input.messages.at(-1)?.role === 'tool') {
                if (input.messages.at(-1)?.tool_call_id === 'html') assert.match(input.messages.at(-1)?.content ?? '', /output truncated/);
                upstreamResponse(res, {content: 'done'}, 'stop');
            } else if (input.messages.at(-1)?.content === 'read HTML') {
                upstreamResponse(res, {tool_calls: [{index: 0, id: 'html', function: {name: 'read_file', arguments: JSON.stringify({target_file: 'large.html'})}}]}, 'tool_calls');
            } else upstreamResponse(res, {tool_calls: [{index: 0, id: 'once', function: {name: 'run_command', arguments: JSON.stringify({command: 'printf x >> count'})}}]}, 'tool_calls');
        }, async upstream => {
            const running = await backend(binary, temp, upstream);
            try {
                for (const mode of ['headers', 'body'] as const) {
                    const ws = path.join(temp, mode);fs.mkdirSync(ws);
                    const requests: RoundRequest[] = [];let lost = false;
                    await server(async (req, res) => {
                        let raw = '';for await (const chunk of req) raw += chunk;
                        const input = JSON.parse(raw) as RoundRequest;requests.push(input);
                        const response = await fetch(running.url + '/chat/stream', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: raw});
                        const text = await response.text();
                        if (!lost) {
                            lost = true;
                            if (mode === 'headers') {res.writeHead(503);res.end();return;}
                            res.setHeader('X-Session-Id', response.headers.get('x-session-id')!);
                            res.setHeader('Content-Type', 'text/event-stream');
                            res.end(text.slice(0, text.lastIndexOf('data: {"type":"done"')));return;
                        }
                        res.writeHead(response.status, {'Content-Type': response.headers.get('content-type')!, 'X-Session-Id': response.headers.get('x-session-id')!});
                        res.end(text);
                    }, async proxy => {
                        const convo = new Conversation(proxy, ws, '', 25, {approvalMode: 'yes', retryPolicy: fast, retryDependencies: {sleep: async () => {}, onRetry: () => {}}});
                        const result = await convo.run('execute once');
                        assert.equal(result.status, 'completed', result.message);
                        assert.equal(fs.readFileSync(path.join(ws, 'count'), 'utf8'), 'x');
                        assert.equal(requests.length, 3);
                        assert.equal(requests[0].roundId, requests[1].roundId);assert.notEqual(requests[1].roundId, requests[2].roundId);
                        assert(requests.every(request => !('messages' in request)));
                        assert.equal(requests[2].toolUseResults?.length, 1);
                        const info = await getSession(running.url, convo.sessionId);
                        assert.equal(info.session.total_tokens, 10);assert.equal(info.session.message_count, 4);
                    });
                }
                assert.equal(modelCalls, 4);assert.equal((await listSessions(running.url)).length, 2);
                const htmlWorkspace = path.join(temp, 'html');fs.mkdirSync(htmlWorkspace);
                fs.writeFileSync(path.join(htmlWorkspace, 'large.html'), '<>&\u2028\u2029'.repeat(5000));
                const html = new Conversation(running.url, htmlWorkspace, '', 25, {approvalMode: 'yes', retryPolicy: fast});
                const htmlResult = await html.run('read HTML');
                assert.equal(htmlResult.status, 'completed', htmlResult.message);
                assert.equal((await getSession(running.url, html.sessionId)).session.message_count, 4);
                const ws = path.join(temp, 'unsafe');fs.mkdirSync(ws);
                const round = await runRound({serverUrl: running.url, sessionId: '', roundId: 'unsafe-first', query: 'unexecuted batch', workspace: ws, model: ''});
                const resumed = await cli(['new query', '--resume', round.sessionId!, '--server', running.url]);
                assert.equal(resumed.code, 1);assert.match(resumed.stderr, /unfinished tool batch/);assert(!fs.existsSync(path.join(ws, 'count')));
            } finally {await running.stop();}
        });
    } finally {fs.rmSync(temp, {recursive: true, force: true});}
});

test('failed tool-result submission retains its delta and never executes the tool twice', async () => {
    const ws = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-result-retry-'));
    const requests: RoundRequest[] = [];let fail = true;
    try {
        const convo = new Conversation('http://unused.invalid', ws, '', 25, {approvalMode: 'yes', retryPolicy: fast, retryDependencies: {run: async input => {
            requests.push(structuredClone({...input, options: undefined}));
            if (input.query) return {...done, finish: 'tool_calls', toolCalls: [{id: 'once', name: 'run_command', arguments: {command: 'printf x >> count'}}]};
            if (fail) throw new RoundError('fatal', 'http', {httpStatus: 400});
            return done;
        }}});
        assert.equal((await convo.run('ask')).status, 'failed');
        fail = false;
        assert.equal((await convo.run('ask')).status, 'completed');
        assert.equal(fs.readFileSync(path.join(ws, 'count'), 'utf8'), 'x');
        assert.equal(requests.length, 3);assert.equal(requests[1].roundId, requests[2].roundId);
        assert.deepEqual(requests[1].toolUseResults, requests[2].toolUseResults);
    } finally {fs.rmSync(ws, {recursive: true, force: true});}
});

test('cancelling a tool stages an error result and in-process retry does not replay its side effect', {timeout: 10_000}, async () => {
    const ws = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-tool-cancel-'));
    const controller = new AbortController();let executed = false;
    try {
        const convo = new Conversation('http://unused.invalid', ws, '', 25, {approvalMode: 'yes', retryPolicy: fast, retryDependencies: {run: async input => {
            if (input.toolUseResults) {assert.equal(input.toolUseResults[0].isError, true);return done;}
            executed = true;
            return {...done, finish: 'tool_calls', toolCalls: [{id: 'once', name: 'run_command', arguments: {command: 'printf x >> count; sleep 5'}}]};
        }}});
        const pending = convo.run('ask', controller.signal);
        const deadline = Date.now() + 5000;
        while (!fs.existsSync(path.join(ws, 'count'))) {
            if (Date.now() > deadline) {controller.abort();await pending;assert.fail('tool did not start');}
            await new Promise(resolve => setTimeout(resolve, 5));
        }
        controller.abort();
        assert.equal((await pending).status, 'cancelled');assert(executed);
        assert.equal((await convo.run('ask')).status, 'completed');assert.equal(fs.readFileSync(path.join(ws, 'count'), 'utf8'), 'x');
    } finally {fs.rmSync(ws, {recursive: true, force: true});}
});

test('session headers are mandatory and cannot silently switch an existing conversation', async () => {
    for (const header of ['', '../unsafe', 's_changed']) await server((req, res) => {
        if (header) res.setHeader('X-Session-Id', header);
        res.setHeader('Content-Type', 'text/event-stream');res.end('data: {"type":"done","finish_reason":"stop"}\n\n');
    }, async url => {
        await assert.rejects(() => runRound({serverUrl: url, sessionId: 's_expected', roundId: 'header', query: 'ask', workspace: '/tmp', model: ''}), (error: unknown) => error instanceof RoundError && error.kind === 'protocol');
    });
});
