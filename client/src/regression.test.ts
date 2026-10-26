import {test} from 'node:test';
import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import * as http from 'node:http';
import {once} from 'node:events';
import {spawn, spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {HandlerContext, createHandler, ToolError} from './tools/index.ts';
import {MAX_FILE_BYTES, MAX_OUTPUT_BYTES, boundedOutput} from './tools/base.ts';
import {Conversation, MAX_HISTORY_BYTES} from './conversation.ts';
import {runRound, runRoundNonStreaming} from './executor.ts';
import {RETRY_POLICY} from './tuning.ts';
import {parseSSE} from './sse.ts';
import type {Event, Message} from './protocol.ts';

const project = fileURLToPath(new URL('../..', import.meta.url));
const events = (...values: Event[]): string => values.map(value => `data: ${JSON.stringify(value)}\n\n`).join('');
const end: Event = {type: 'done', finish_reason: 'stop'};

async function fixture(fn: (workspace: string, ctx: HandlerContext, temp: string) => Promise<void> | void): Promise<void> {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-test-'));
    const workspace = path.join(temp, 'workspace'); fs.mkdirSync(workspace);
    try { await fn(workspace, new HandlerContext(workspace), temp); }
    finally { fs.rmSync(temp, {recursive: true, force: true}); }
}

async function server(fn: http.RequestListener, run: (url: string) => Promise<void>): Promise<void> {
    const listener = http.createServer((req, res) => {res.setHeader('X-Session-Id', 's_test');fn(req, res);});
    listener.listen(0, '127.0.0.1'); await once(listener, 'listening');
    try { await run(`http://127.0.0.1:${(listener.address() as import('node:net').AddressInfo).port}`); }
    finally { listener.closeAllConnections(); await new Promise<void>(resolve => listener.close(() => resolve())); }
}

test('file guards reject traversal, file/directory/dangling symlinks without changing external fixtures', () => fixture(async (ws, ctx, temp) => {
    const outside = path.join(temp, 'outside'); fs.mkdirSync(outside);
    const file = path.join(outside, 'file'); fs.writeFileSync(file, 'original');
    fs.symlinkSync(file, path.join(ws, 'link'));
    fs.symlinkSync(outside, path.join(ws, 'dir'));
    fs.symlinkSync(path.join(outside, 'missing'), path.join(ws, 'dangling'));
    for (const target of ['../outside/file', file, 'link', 'dir/file', 'dangling']) {
        assert.throws(() => createHandler('read_file', ctx)!.execute({target_file: target}), ToolError);
        assert.throws(() => createHandler('write_file', ctx)!.execute({target_file: target, content: 'bad'}), ToolError);
        assert.throws(() => createHandler('edit_file', ctx)!.execute({target_file: target, old_string: 'original', new_string: 'bad'}), ToolError);
    }
    assert.throws(() => createHandler('write_file', ctx)!.execute({target_file: 'dir/new/nested', content: 'bad'}), ToolError);
    assert.throws(() => createHandler('list_dir', ctx)!.execute({target_directory: 'dir'}), ToolError);
    await assert.rejects(async () => createHandler('grep', ctx)!.execute({path: 'dir', pattern: 'original'}), ToolError);
    const result = await createHandler('grep', ctx)!.execute({pattern: 'original'});
    assert.equal(result, 'No matches.');
    assert.equal(fs.readFileSync(file, 'utf8'), 'original');
    assert(!fs.existsSync(path.join(outside, 'new')));
}));

test('editing preserves dollar sequences, rejects empty and overlapping ambiguous matches, and preserves mode', () => fixture((ws, ctx) => {
    const file = path.join(ws, 'edit'); fs.writeFileSync(file, 'before TOKEN after', {mode: 0o755});
    createHandler('edit_file', ctx)!.execute({target_file: 'edit', old_string: 'TOKEN', new_string: "$& $$ $` $'"});
    assert.equal(fs.readFileSync(file, 'utf8'), "before $& $$ $` $' after");
    assert.equal(fs.statSync(file).mode & 0o777, 0o755);
    assert.throws(() => createHandler('edit_file', ctx)!.execute({target_file: 'edit', old_string: '', new_string: 'x'}), ToolError);
    fs.writeFileSync(file, 'aaa');
    assert.throws(() => createHandler('edit_file', ctx)!.execute({target_file: 'edit', old_string: 'aa', new_string: 'x'}), /not unique/);
}));

test('runtime validation and registry reject invalid values before mutation', () => fixture(async (ws, ctx) => {
    for (const content of [null, 1, {}, [], true]) {
        assert.throws(() => createHandler('write_file', ctx)!.execute({target_file: 'file', content}), ToolError);
    }
    assert(!fs.existsSync(path.join(ws, 'file')));
    for (const name of ['toString', '__proto__', 'constructor', 'unknown']) assert.equal(createHandler(name, ctx), null);
    assert.throws(() => createHandler('run_command', ctx)!.execute({command: 123}), ToolError);
    for (const limit of [null, '2', -1, 0, NaN, 2001]) {
        assert.throws(() => createHandler('read_file', ctx)!.execute({target_file: 'file', limit}), ToolError);
    }
    const schemas = fs.readdirSync(path.join(project, 'backend/tools'));
    for (const file of schemas) {
        const schema = JSON.parse(fs.readFileSync(path.join(project, 'backend/tools', file), 'utf8'));
        assert.equal(createHandler(schema.function.name, ctx)?.name, schema.function.name);
    }
    const convo = new Conversation('http://127.0.0.1:1', ws);
    assert.match(await convo.runTool({id: 'x', name: 'constructor', arguments: {}}), /^ERROR: unknown tool/);
}));

test('file reads, writes and serialized tool results have explicit budgets', () => fixture((ws, ctx) => {
    fs.writeFileSync(path.join(ws, 'large'), 'x'.repeat(MAX_FILE_BYTES + 1));
    assert.throws(() => createHandler('read_file', ctx)!.execute({target_file: 'large', limit: 1}), /exceeds/);
    assert.throws(() => createHandler('write_file', ctx)!.execute({target_file: 'new', content: 'x'.repeat(MAX_FILE_BYTES + 1)}), /exceeds/);
    assert(!fs.existsSync(path.join(ws, 'new')));
    fs.writeFileSync(path.join(ws, 'long'), '中'.repeat(100000));
    const result = createHandler('read_file', ctx)!.execute({target_file: 'long', limit: 1}) as string;
    assert(Buffer.byteLength(result) <= MAX_OUTPUT_BYTES);
    assert.match(result, /truncated/);
    assert(Buffer.byteLength(JSON.stringify(boundedOutput('\n'.repeat(MAX_OUTPUT_BYTES)))) <= MAX_OUTPUT_BYTES);
}));

test('commands are asynchronous, preserve exit codes, enforce output limits and cancel descendants', () => fixture(async (ws, ctx) => {
    let ticked = false;
    setTimeout(() => { ticked = true; }, 0);
    const output = await createHandler('run_command', ctx)!.execute({command: "sleep 0.05; echo ok; exit 7"});
    assert(ticked); assert.match(output, /exit_code: 7/); assert.match(output, /ok/);
    await assert.rejects(async () => createHandler('run_command', ctx)!.execute({command: `"${process.execPath}" -e "process.stdout.write('x'.repeat(100000))"`}), /output exceeds/);
    const controller = new AbortController(); ctx.signal = controller.signal;
    const command = createHandler('run_command', ctx)!.execute({command: '(sleep 1; echo bad > descendant-marker) & wait'});
    setTimeout(() => controller.abort(), 40);
    await assert.rejects(async () => command, /cancelled/);
    await new Promise(resolve => setTimeout(resolve, 1100));
    assert(!fs.existsSync(path.join(ws, 'descendant-marker')));
}));

test('pathological regex runs in a cancellable worker', () => fixture(async (ws, ctx) => {
    fs.writeFileSync(path.join(ws, 'text'), 'a'.repeat(100) + '!');
    const controller = new AbortController(); ctx.signal = controller.signal;
    const search = createHandler('grep', ctx)!.execute({pattern: '(a+)+$'});
    setTimeout(() => controller.abort(), 100);
    await assert.rejects(async () => search, /cancelled/);
}));

test('SSE parses split UTF-8/multiline frames and rejects malformed/incomplete/oversize data', async () => {
    const bytes = Buffer.from('data: {"type":"text",\ndata: "delta":"中文"}\r\n\r\n');
    async function* pieces(): AsyncGenerator<Uint8Array> { for (const byte of bytes) yield Buffer.from([byte]); }
    const parsed = []; for await (const event of parseSSE(pieces())) parsed.push(event);
    assert.equal(parsed[0].delta, '中文');
    for (const raw of ['data: nope\n\n', 'data: {"type":"text"}\n', 'data: []\n\n', 'x'.repeat(1024 * 1024 + 1)]) {
        async function consume(): Promise<void> { for await (const ignored of parseSSE((async function* () { yield Buffer.from(raw); })())) void ignored; }
        await assert.rejects(consume);
    }
});

for (const [name, code, type, body] of [
    ['HTTP failure', 500, 'application/json', '{}'],
    ['wrong content type', 200, 'application/json', '{}'],
    ['backend error', 200, 'text/event-stream', events({type: 'error', message: 'failed', code: 'upstream_5xx', retryable: true}, end)],
    ['missing done', 200, 'text/event-stream', events({type: 'text', delta: 'partial'})],
    ['unsuccessful finish', 200, 'text/event-stream', events({type: 'done', finish_reason: 'length'})],
    ['missing tools', 200, 'text/event-stream', events({type: 'done', finish_reason: 'tool_calls'})],
] as const) {
    test(`round rejects ${name}`, () => server((req, res) => {res.writeHead(code, {'Content-Type': type}); res.end(body);}, async url => {
        await assert.rejects(() => runRound({serverUrl: url, sessionId: '', roundId: 'test', query: 'test', workspace: '/tmp', model: ''}));
    }));
}

test('conversation distinguishes failure, exhaustion, cancellation and budget rejection', () => fixture(async (ws) => {
    const call: Event = {type: 'tool_call', id: 'call', name: 'write_file', arguments: {target_file: 'untouched', content: 'bad'}};
    await server((req, res) => {res.setHeader('Content-Type', 'text/event-stream');res.end(events(call, {type: 'done', finish_reason: 'tool_calls'}));}, async url => {
        const convo = new Conversation(url, ws, '', 1);
        assert.equal((await convo.run('test')).status, 'exhausted');
        assert(!fs.existsSync(path.join(ws, 'untouched')));
        assert(!('messages' in convo));
        const budget = new Conversation(url, ws);
        assert.equal((await budget.run('x'.repeat(MAX_HISTORY_BYTES))).status, 'failed');
        assert(!('messages' in budget));
        const controller = new AbortController(); controller.abort();
        assert.equal((await convo.run('cancel', controller.signal)).status, 'cancelled');
    });
    await server((req, res) => {res.setHeader('Content-Type', 'text/event-stream');res.end(events({type: 'error', message: 'broken'}));}, async url => {
        assert.equal((await new Conversation(url, ws).run('test')).status, 'failed');
    });
}));

test('round cancellation interrupts a stalled response', () => server((req, res) => {
    res.writeHead(200, {'Content-Type': 'text/event-stream'});res.flushHeaders();
}, async url => {
    const controller = new AbortController();
    const pending = runRound({serverUrl: url, sessionId: '', roundId: 'test', query: 'test', workspace: '/tmp', model: ''}, controller.signal);
    setTimeout(() => controller.abort(), 40);
    await assert.rejects(() => pending);
}));

async function cli(args: string[], input?: string): Promise<{code: number | null; stdout: string; stderr: string}> {
    const child = spawn(path.join(project, 'run_client.sh'), args, {stdio: ['pipe', 'pipe', 'pipe']});
    let stdout = '', stderr = '';
    child.stdout.on('data', chunk => {stdout += chunk;}); child.stderr.on('data', chunk => {stderr += chunk;});
    const exit = once(child, 'close'); child.stdin.end(input);
    const [code] = await exit;
    return {code, stdout, stderr};
}

test('CLI consumes all piped queries, returns failures as nonzero, and validates options', () => fixture(async ws => {
    await server((req, res) => {res.setHeader('Content-Type', 'text/event-stream');res.end(events({type: 'text', delta: 'ok'}, end));}, async url => {
        const result = await cli(['--workspace', ws, '--server', url], 'first\nsecond\nexit\n');
        assert.equal(result.code, 0, result.stderr); assert.equal(result.stdout.match(/=== answer ===/g)?.length, 2);
    });
    await server((req, res) => {res.writeHead(400);res.end();}, async url => {
        assert.equal((await cli(['query', '--workspace', ws, '--server', url])).code, 1);
    });
    for (const args of [['--workspace'], ['--max-rounds', 'NaN'], ['--max-rounds', '0'], ['--unknown']]) {
        const result = await cli(args); assert.equal(result.code, 1); assert(!result.stdout.includes('=== answer ==='));
    }
}));

test('Go proxy and real CLI complete read → edit → command → final response', {timeout: 30_000}, () => fixture(async (ws, ctx, temp) => {
    const binary = path.join(temp, 'backend');
    const build = spawnSync('go', ['build', '-o', binary, '.'], {cwd: path.join(project, 'backend'), encoding: 'utf8'});
    assert.equal(build.status, 0, build.stderr);
    fs.writeFileSync(path.join(ws, 'calc.mjs'), 'export const add = (a, b) => a - b;\n');
    fs.writeFileSync(path.join(ws, 'calc.test.mjs'), "import {test} from 'node:test'; import assert from 'node:assert/strict'; import {add} from './calc.mjs'; test('adds', () => assert.equal(add(2, 3), 5));\n");
    const received: Message[][] = [];
    await server(async (req, res) => {
        let data = '';for await (const chunk of req) data += chunk;
        const payload = JSON.parse(data) as {messages: Message[]}; received.push(payload.messages);
        const last = payload.messages.at(-1)!;
        let delta: unknown, finish = 'tool_calls';
        const tool = (id: string, name: string, args: unknown): unknown => ({tool_calls: [{index: 0, id, function: {name, arguments: JSON.stringify(args)}}]});
        if (last.role === 'user') delta = tool('read', 'read_file', {target_file: 'calc.mjs'});
        else if (last.tool_call_id === 'read') delta = tool('edit', 'edit_file', {target_file: 'calc.mjs', old_string: 'a - b', new_string: 'a + b'});
        else if (last.tool_call_id === 'edit') delta = tool('test', 'run_command', {command: `"${process.execPath}" --test calc.test.mjs`});
        else {delta = {content: last.content?.includes('exit_code: 0') ? 'fixed and tests passed' : 'tests failed'}; finish = 'stop';}
        res.setHeader('Content-Type', 'text/event-stream');
        res.end(`data: ${JSON.stringify({choices: [{delta, finish_reason: finish}]})}\n\ndata: [DONE]\n\n`);
    }, async upstream => {
        const reservation = http.createServer(); reservation.listen(0, '127.0.0.1'); await once(reservation, 'listening');
        const port = (reservation.address() as import('node:net').AddressInfo).port; await new Promise<void>(resolve => reservation.close(() => resolve()));
        const child = spawn(binary, [], {cwd: temp, env: {...process.env, STORE_BACKEND: 'file', MINICOCO_MODEL_IS_FUNCTION_CALL: '', MINICOCO_MODELS_FILE: '', MINICOCO_DEBUG_PROMPT: '0', MINICOCO_DATA_DIR: path.join(temp, 'data'), LLM_PROVIDER: 'openai', LLM_BASE_URL: upstream, LLM_API_KEY: 'fake-test-key', LLM_MODEL: 'deepseek-flash', MINICOCO_HOST: '127.0.0.1', MINICOCO_PORT: String(port)}, stdio: 'ignore'});
        const exit = once(child, 'exit');
        try {
            let ready = false;
            for (let i = 0; i < 100; i++) {
                try {if ((await fetch(`http://127.0.0.1:${port}/health`)).ok) {ready = true;break;}} catch {}
                await new Promise(resolve => setTimeout(resolve, 20));
            }
            assert(ready, 'backend did not start');
            const result = await cli(['fix calc', '--workspace', ws, '--server', `http://127.0.0.1:${port}`]);
            assert.equal(result.code, 0, result.stderr); assert.match(result.stdout, /fixed and tests passed/);
            assert.match(fs.readFileSync(path.join(ws, 'calc.mjs'), 'utf8'), /a \+ b/);
            assert.equal(received.length, 4);
            assert.equal(received[3].filter(message => message.role === 'tool').length, 3);
        } finally {child.kill('SIGTERM');await exit;}
    });
}));

test('Go flaky backend and real CLI recover with default cancellable backoff', {timeout: 30_000}, () => fixture(async (ws, ctx, temp) => {
    const binary = path.join(temp, 'backend');
    const build = spawnSync('go', ['build', '-o', binary, '.'], {cwd: path.join(project, 'backend'), encoding: 'utf8'});
    assert.equal(build.status, 0, build.stderr);
    const reservation = http.createServer();reservation.listen(0, '127.0.0.1');await once(reservation, 'listening');
    const port = (reservation.address() as import('node:net').AddressInfo).port;await new Promise<void>(resolve => reservation.close(() => resolve()));
    const child = spawn(binary, [], {cwd: temp, env: {...process.env, STORE_BACKEND: 'file', MINICOCO_MODEL_IS_FUNCTION_CALL: '', MINICOCO_MODELS_FILE: '', MINICOCO_DEBUG_PROMPT: '0', MINICOCO_DATA_DIR: path.join(temp, 'data'), LLM_PROVIDER: 'mock', LLM_API_KEY: '', LLM_MOCK_MODE: 'flaky', LLM_MOCK_FLAKY_FAILS: '2', MINICOCO_HOST: '127.0.0.1', MINICOCO_PORT: String(port)}, stdio: 'ignore'});
    const exit = once(child, 'exit');
    try {
        let ready = false;
        for (let i = 0; i < 100; i++) {
            try {if ((await fetch(`http://127.0.0.1:${port}/health`)).ok) {ready = true;break;}} catch {}
            await new Promise(resolve => setTimeout(resolve, 20));
        }
        assert(ready);
        const result = await cli(['inspect workspace', '--workspace', ws, '--server', `http://127.0.0.1:${port}`]);
        assert.equal(result.code, 0, result.stderr);assert.match(result.stdout, /Done\. I inspected/);
        assert.equal(result.stderr.match(/\[retry\]/g)?.length, 2);
        assert.equal(result.stdout.match(/→ list_dir/g)?.length, 1);
    } finally {child.kill('SIGTERM');await exit;}
}));

test('Go flaky backend recovers through one real JSON fallback and executes a tool once', {timeout: 30_000}, () => fixture(async (ws, ctx, temp) => {
    const binary = path.join(temp, 'backend');
    const build = spawnSync('go', ['build', '-o', binary, '.'], {cwd: path.join(project, 'backend'), encoding: 'utf8'});
    assert.equal(build.status, 0, build.stderr);
    const reservation = http.createServer();reservation.listen(0, '127.0.0.1');await once(reservation, 'listening');
    const port = (reservation.address() as import('node:net').AddressInfo).port;await new Promise<void>(resolve => reservation.close(() => resolve()));
    const child = spawn(binary, [], {cwd: temp, env: {...process.env, STORE_BACKEND: 'file', MINICOCO_MODEL_IS_FUNCTION_CALL: '', MINICOCO_MODELS_FILE: '', MINICOCO_DEBUG_PROMPT: '0', MINICOCO_DATA_DIR: path.join(temp, 'data'), LLM_PROVIDER: 'mock', LLM_API_KEY: '', LLM_MOCK_MODE: 'flaky', LLM_MOCK_FLAKY_FAILS: '2', MINICOCO_HOST: '127.0.0.1', MINICOCO_PORT: String(port)}, stdio: 'ignore'});
    const exit = once(child, 'exit');let fallbacks = 0;
    try {
        let ready = false;
        for (let i = 0; i < 100; i++) {
            try {if ((await fetch(`http://127.0.0.1:${port}/health`)).ok) {ready = true;break;}} catch {}
            await new Promise(resolve => setTimeout(resolve, 20));
        }
        assert(ready);
        const convo = new Conversation(`http://127.0.0.1:${port}`, ws, '', 25, {
            retryPolicy: {...RETRY_POLICY, maxRetries: 1, baseMs: 0, capMs: 0, jitter: 0},
            retryDependencies: {onRetry: () => {}, runNonStreaming: async (input, signal) => {
                fallbacks++;return runRoundNonStreaming(input, signal);
            }},
        });
        const outcome = await convo.run('inspect workspace');assert.equal(outcome.status, 'completed', outcome.message);
        assert.equal(fallbacks, 1);assert(!('messages' in convo));assert.match(outcome.text, /Done\. I inspected/);
    } finally {child.kill('SIGTERM');await exit;}
}));

test('trace header survives SSE and JSON failures for diagnosis', async () => {
    for (const stream of [true, false]) await server((_req, res) => {
        res.setHeader('X-Trace-Id', 's_test-r3');
        const failure: Event = {type: 'error', message: 'failed', code: 'bad_request', retryable: false};
        res.setHeader('Content-Type', stream ? 'text/event-stream' : 'application/json');
        res.end(stream ? events(failure) : JSON.stringify({events: [failure]}));
    }, async url => {
        await assert.rejects((stream ? runRound : runRoundNonStreaming)({serverUrl: url, sessionId: 's_test', roundId: 'trace', query: 'q', workspace: '/tmp', model: ''}), (error: unknown) => {
            assert.equal((error as {traceId: string}).traceId, 's_test-r3');
            return true;
        });
    });
});

test('trace CLI prints a filtered timeline and observed usage', async () => {
    await server((req, res) => {
        assert.equal(req.url, '/trace/s_test?round=3');
        res.setHeader('Content-Type', 'application/json');
        res.end(JSON.stringify({records: [{round: 3, traceId: 's_test-r3', stage: 'llm_response', ts: 1, usage: {total: 2}}, {round: 3, traceId: 's_test-r3', stage: 'error', ts: 2, code: 'bad_request'}]}));
    }, async url => {
        const child = spawn(process.execPath, [path.join(project, 'client/src/cli.ts'), 'trace', 's_test', '--round', '3', '--server', url]);
        let output = '';child.stdout.on('data', chunk => {output += chunk;});
        assert.equal((await once(child, 'close'))[0], 0);
        assert.match(output, /round 3/);assert.match(output, /error\{bad_request\}/);assert.match(output, /observed tokens=2/);
    });
});
