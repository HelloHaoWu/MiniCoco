import {test} from 'node:test';
import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import * as http from 'node:http';
import {once} from 'node:events';
import {spawn} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {Conversation} from './conversation.ts';
import {consumeEvents, runRound, runRoundNonStreaming, RoundError} from './executor.ts';
import {runRoundWithRetry} from './retry.ts';
import {RETRY_POLICY} from './tuning.ts';
import type {ToolMode, ToolCall, Event, RoundRequest} from './protocol.ts';

const args = {serverUrl: 'http://unused.invalid', sessionId: '', roundId: 'mode-round', query: 'inspect', workspace: '/tmp', model: ''};
const done: Event = {type: 'done', finish_reason: 'tool_calls'};
const call = (name: string): Event => ({type: 'tool_call', id: 'one', name, arguments: {}});

async function server(handler: http.RequestListener, run: (url: string) => Promise<void>): Promise<void> {
    const instance = http.createServer((req, res) => {if (req.url === '/audit') {res.writeHead(202);res.end('{}');return;}res.setHeader('X-Session-Id', 's_mode');handler(req, res);});
    instance.listen(0, '127.0.0.1');await once(instance, 'listening');
    try {await run(`http://127.0.0.1:${(instance.address() as import('node:net').AddressInfo).port}`);}
    finally {instance.closeAllConnections();await new Promise<void>(resolve => instance.close(() => resolve()));}
}

async function cli(options: string[]): Promise<{code: number | null; stdout: string; stderr: string}> {
    const entry = fileURLToPath(new URL('./cli.ts', import.meta.url));
    const child = spawn(process.execPath, [entry, ...options], {stdio: ['pipe', 'pipe', 'pipe']});
    let stdout = '', stderr = '';
    child.stdout.on('data', chunk => {stdout += chunk;});child.stderr.on('data', chunk => {stderr += chunk;});
    const closed = once(child, 'close');child.stdin.end();const [code] = await closed;
    return {code, stdout, stderr};
}

test('both event consumers enforce full/readonly/safe tool policy', async () => {
    for (const mode of ['full', 'readonly', 'safe'] as const) {
        const allowed = mode === 'full' ? ['read_file', 'list_dir', 'grep', 'write_file', 'edit_file', 'run_command'] :
            mode === 'safe' ? ['read_file', 'list_dir', 'grep', 'write_file', 'edit_file'] : ['read_file', 'list_dir', 'grep'];
        for (const name of ['read_file', 'list_dir', 'grep', 'write_file', 'edit_file', 'run_command', 'unknown']) {
            const events = [call(name), done];
            const forms = [events, (async function* () {yield* events;})()];
            for (const source of forms) {
                if (allowed.includes(name)) assert.equal((await consumeEvents(source, mode)).toolCalls[0].name, name);
                else await assert.rejects(() => consumeEvents(source, mode), (error: unknown) => error instanceof RoundError && error.kind === 'protocol');
            }
        }
    }
});

test('SSE and JSON carry mode and reject forbidden calls before tool execution', async () => {
    const received: Array<RoundRequest & {stream: boolean}> = [];
    await server(async (req, res) => {
        let raw = '';for await (const chunk of req) raw += chunk;
        const input = JSON.parse(raw) as RoundRequest & {stream: boolean};received.push(input);
        const events = [call('run_command'), done];
        res.setHeader('Content-Type', input.stream ? 'text/event-stream' : 'application/json');
        res.end(input.stream ? events.map(event => `data: ${JSON.stringify(event)}\n\n`).join('') : JSON.stringify({events}));
    }, async url => {
        for (const run of [runRound, runRoundNonStreaming]) {
            for (const mode of ['readonly', 'safe'] as const) {
                await assert.rejects(() => run({...args, serverUrl: url, mode}), /not allowed in mode/);
                assert.equal(received.at(-1)?.mode, mode);
            }
            assert.equal((await run({...args, serverUrl: url, mode: 'full'})).toolCalls[0].name, 'run_command');
        }
    });
    assert.deepEqual(received.map(request => request.stream), [true, true, true, false, false, false]);
});

test('conversation checks an entire tool batch before side effects and fixes its mode for the run', async () => {
    const workspace = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-mode-'));
    try {
        for (const mode of ['readonly', 'safe'] as const) {
            const calls: ToolCall[] = [
                {id: 'write', name: 'write_file', arguments: {target_file: 'should-not-exist', content: 'bad'}},
                {id: 'exec', name: 'run_command', arguments: {command: 'printf x >> side-effect'}},
            ];
            const options = {mode: mode as ToolMode, retryDependencies: {run: async (request: RoundRequest) => {
                assert.equal(request.mode, mode);
                return {sessionId: 's_mode', assistant: {role: 'assistant'}, finish: 'tool_calls', toolCalls: calls};
            }}};
            const convo = new Conversation('http://unused.invalid', workspace, '', 25, options);
            options.mode = 'full'; // Caller mutation cannot widen this conversation.
            const result = await convo.run('try writing');
            assert.equal(result.status, 'failed');assert.match(result.message!, /not allowed in mode/);
            assert.equal(fs.existsSync(path.join(workspace, 'should-not-exist')), false);
            assert.equal(fs.existsSync(path.join(workspace, 'side-effect')), false);
        }
        const safe = new Conversation('http://unused.invalid', workspace, '', 25, {mode: 'safe', approvalMode: 'yes'});
        assert.doesNotMatch(await safe.runTool({id: 'w', name: 'write_file', arguments: {target_file: 'allowed', content: 'safe write'}}), /^ERROR:/);
        assert.equal(fs.readFileSync(path.join(workspace, 'allowed'), 'utf8'), 'safe write');
        assert.match(await safe.runTool({id: 'x', name: 'run_command', arguments: {command: 'printf x >> side-effect'}}), /^ERROR:.*not allowed/);
        const readonly = new Conversation('http://unused.invalid', workspace, '', 25, {mode: 'readonly'});
        assert.match(await readonly.runTool({id: 'r', name: 'read_file', arguments: {target_file: 'allowed'}}), /safe write/);
        assert.match(await readonly.runTool({id: 'e', name: 'edit_file', arguments: {target_file: 'allowed', old_string: 'safe', new_string: 'bad'}}), /^ERROR:.*not allowed/);
        assert.equal(fs.readFileSync(path.join(workspace, 'allowed'), 'utf8'), 'safe write');
    } finally {fs.rmSync(workspace, {recursive: true, force: true});}
});

test('mode remains fixed across retries and JSON fallback', async () => {
    const received: ToolMode[] = [];
    const input = {...args, mode: 'readonly' as ToolMode};
    const outcome = await runRoundWithRetry(input, {...RETRY_POLICY, maxRetries: 1}, undefined, {
        run: async current => {
            received.push(current.mode!);current.mode = 'full';input.mode = 'safe';
            throw new RoundError('temporary', 'network');
        },
        runNonStreaming: async current => {
            received.push(current.mode!);
            return {sessionId: 's_mode', assistant: {role: 'assistant', content: 'done'}, toolCalls: [], finish: 'stop'};
        }, sleep: async () => {}, onRetry: () => {},
    });
    assert.equal(outcome.attempts, 3);assert.deepEqual(received, ['readonly', 'readonly', 'readonly']);
});

test('CLI validates --mode and propagates it on every round', async () => {
    const workspace = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-mode-cli-'));
    const received: Array<RoundRequest> = [];
    try {
        await server(async (req, res) => {
            let raw = '';for await (const chunk of req) raw += chunk;
            const input = JSON.parse(raw) as RoundRequest;received.push(input);
            const events = input.query ? [{type: 'tool_call', id: 'list', name: 'list_dir', arguments: {target_directory: '.'}}, done] : [{type: 'text', delta: 'finished'}, {type: 'done', finish_reason: 'stop'}];
            res.setHeader('Content-Type', 'text/event-stream');res.end(events.map(event => `data: ${JSON.stringify(event)}\n\n`).join(''));
        }, async url => {
            for (const mode of ['readonly', 'safe', 'full'] as const) {
                const result = await cli(['inspect', '--workspace', workspace, '--server', url, '--mode', mode]);
                assert.equal(result.code, 0, result.stderr);assert.match(result.stdout, /finished/);
                assert.deepEqual(received.slice(-2).map(request => request.mode), [mode, mode]);
            }
            const defaults = await cli(['inspect', '--workspace', workspace, '--server', url]);
            assert.equal(defaults.code, 0, defaults.stderr);assert(received.slice(-2).every(request => request.mode === 'full'));
            const before = received.length;
            for (const mode of ['invalid', 'READONLY']) {
                const rejected = await cli(['inspect', '--server', url, '--mode', mode]);
                assert.equal(rejected.code, 1);assert.match(rejected.stderr, /mode must be/);
            }
            assert.equal((await cli(['--mode'])).code, 1);
            assert.equal(received.length, before);
        });
    } finally {fs.rmSync(workspace, {recursive: true, force: true});}
});
