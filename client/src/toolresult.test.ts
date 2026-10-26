import {test} from 'node:test';
import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import * as http from 'node:http';
import {once} from 'node:events';
import {spawn, spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {getSession, listSessions} from './sessions.ts';
import {runRoundWithRetry} from './retry.ts';
import {RoundError} from './roundError.ts';

const project = fileURLToPath(new URL('../..', import.meta.url));

async function cli(args: string[]): Promise<{code: number | null; stdout: string; stderr: string}> {
    const child = spawn(process.execPath, [path.join(project, 'client/src/cli.ts'), ...args], {stdio: ['pipe', 'pipe', 'pipe']});
    let stdout = '', stderr = '';
    child.stdout.on('data', chunk => {stdout += chunk;});child.stderr.on('data', chunk => {stderr += chunk;});
    const closed = once(child, 'close');child.stdin.end();const [code] = await closed;
    return {code, stdout, stderr};
}

async function backend(binary: string, dir: string, model: string, loop = ''): Promise<{url: string; stop: () => Promise<void>}> {
    const reservation = http.createServer();reservation.listen(0, '127.0.0.1');await once(reservation, 'listening');
    const port = (reservation.address() as import('node:net').AddressInfo).port;
    await new Promise<void>(resolve => reservation.close(() => resolve()));
    const child = spawn(binary, [], {cwd: dir, env: {...process.env, STORE_BACKEND: 'file', MINICOCO_DATA_DIR: path.join(dir, 'data'),
        LOOP_IMPL: loop, MINICOCO_MODEL_IS_FUNCTION_CALL: '', MINICOCO_MODELS_FILE: '', LLM_MODEL: model, MINICOCO_DEBUG_PROMPT: '0', LLM_PROVIDER: 'mock', LLM_API_KEY: '', LLM_MOCK_MODE: 'normal',
        MINICOCO_HOST: '127.0.0.1', MINICOCO_PORT: String(port)}, stdio: 'ignore'});
    const exited = once(child, 'exit');
    const stop = async (): Promise<void> => {if (child.exitCode === null) child.kill('SIGTERM');await exited;};
    const url = `http://127.0.0.1:${port}`;
    for (let i = 0; i < 100; i++) {
        try {if ((await fetch(url + '/health')).ok) return {url, stop};} catch {}
        if (child.exitCode !== null) break;
        await new Promise(resolve => setTimeout(resolve, 20));
    }
    await stop();throw new Error('formatter backend did not start');
}

test('routed Native/XML and Claude-family mocks persist/resume; unknown models are fatal', {timeout: 30_000}, async () => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-format-cli-'));
    try {
        const binary = path.join(temp, 'backend');
        const build = spawnSync('go', ['build', '-o', binary, '.'], {cwd: path.join(project, 'backend'), encoding: 'utf8'});
        assert.equal(build.status, 0, build.stderr);
        for (const model of ['mock', 'mock-xml', 'mock-claude']) {
            const native = model !== 'mock-xml';
            const dir = path.join(temp, model);fs.mkdirSync(dir);
            const workspace = path.join(dir, 'workspace');fs.mkdirSync(workspace);
            let running = await backend(binary, dir, model);
            try {
                const first = await cli(['inspect', '--server', running.url, '--workspace', workspace, '--mode', 'readonly']);
                assert.equal(first.code, 0, first.stderr);assert.match(first.stdout, /Done\. I inspected/);
                const sid = first.stderr.match(/\[session\] (s_[a-f0-9]+)/)?.[1];assert(sid, first.stderr);
                const transcript = path.join(dir, 'data/transcripts', sid + '.jsonl');
                const rows = fs.readFileSync(transcript, 'utf8').trim().split('\n').map(line => JSON.parse(line) as {role: string; content?: string; request_hash?: string});
                const inputs = rows.filter(row => !row.request_hash);
                if (native) assert(inputs.some(row => row.role === 'tool'));
                else {
                    assert(inputs.some(row => row.role === 'user' && row.content?.startsWith('<tool_result><status>succeeded</status>')));
                    assert(!inputs.some(row => row.role === 'tool'));
                }
                const info = await getSession(running.url, sid);
                assert.equal(info.session.message_count, 4);assert.equal(info.session.model, model);
                await assert.rejects(() => runRoundWithRetry({serverUrl: running.url, sessionId: '', roundId: 'unknown-model', query: 'inspect', workspace, model: 'unknown'}, undefined, undefined, {onRetry: () => assert.fail('unknown model retried')}),
                    (error: unknown) => error instanceof RoundError && error.httpStatus === 400 && error.attempts === 1);
                assert.equal((await listSessions(running.url)).length, 1, 'unknown model created a session');
                await running.stop();running = await backend(binary, dir, model);
                const resumed = await cli(['again', '--server', running.url, '--resume', sid, '--mode', 'readonly']);
                assert.equal(resumed.code, 0, resumed.stderr);
                assert.equal((await getSession(running.url, sid)).session.message_count, 8);
                const listed = await cli(['--list', '--server', running.url]);assert.equal(listed.code, 0, listed.stderr);assert.match(listed.stdout, new RegExp(sid));
            } finally {await running.stop();}
        }
    } finally {fs.rmSync(temp, {recursive: true, force: true});}
});

test('default, explicit v1 and unregistered loop selections complete the real mock CLI', {timeout: 30_000}, async () => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-loop-cli-'));
    try {
        const binary = path.join(temp, 'backend');
        const build = spawnSync('go', ['build', '-o', binary, '.'], {cwd: path.join(project, 'backend'), encoding: 'utf8'});
        assert.equal(build.status, 0, build.stderr);
        for (const loop of ['', 'v1', 'unregistered']) {
            const dir = path.join(temp, loop || 'default');fs.mkdirSync(dir);
            const workspace = path.join(dir, 'workspace');fs.mkdirSync(workspace);
            const running = await backend(binary, dir, 'mock', loop);
            try {
                const health = await (await fetch(running.url + '/health')).json() as {loop_impl: string};
                assert.equal(health.loop_impl, 'v1');
                const result = await cli(['inspect', '--server', running.url, '--workspace', workspace, '--mode', 'readonly']);
                assert.equal(result.code, 0, result.stderr);assert.match(result.stdout, /Done\. I inspected/);
                const sid = result.stderr.match(/\[session\] (s_[a-f0-9]+)/)?.[1];assert(sid, result.stderr);
                const info = await getSession(running.url, sid);
                assert.equal(info.session.message_count, 4);assert.equal(info.session.total_tokens, 4);
            } finally {await running.stop();}
        }
    } finally {fs.rmSync(temp, {recursive: true, force: true});}
});

test('v2 compresses long CLI sessions and resumes checkpoints after backend restart', {timeout: 30_000}, async () => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-compress-cli-'));
    try {
        const binary = path.join(temp, 'backend');
        const build = spawnSync('go', ['build', '-o', binary, '.'], {cwd: path.join(project, 'backend'), encoding: 'utf8'});
        assert.equal(build.status, 0, build.stderr);
        const workspace = path.join(temp, 'workspace');fs.mkdirSync(workspace);
        let running = await backend(binary, temp, 'mock', 'v2');
        try {
            const query = 'remember-this-path project/src/main.go ' + 'x'.repeat(24_000);
            const first = await cli([query, '--server', running.url, '--workspace', workspace]);
            assert.equal(first.code, 0, first.stderr);
            const sid = first.stderr.match(/\[session\] (s_[a-f0-9]+)/)?.[1];assert(sid, first.stderr);
            const second = await cli([query, '--resume', sid, '--server', running.url]);
            assert.equal(second.code, 0, second.stderr);
            const summaryPath = path.join(temp, 'data/transcripts', sid + '.summary.jsonl');
            const summaries = fs.readFileSync(summaryPath, 'utf8').trim().split('\n').map(line => JSON.parse(line) as {prefix_count: number; content: string});
            assert(summaries.some(summary => summary.prefix_count >= 3 && summary.content.includes('Mock context summary')));
            const canonical = fs.readFileSync(path.join(temp, 'data/transcripts', sid + '.jsonl'), 'utf8');
            assert(canonical.includes('remember-this-path project/src/main.go'), 'compression erased the original transcript');
            await running.stop();running = await backend(binary, temp, 'mock', 'v2');
            const third = await cli(['continue after restart', '--resume', sid, '--server', running.url]);
            assert.equal(third.code, 0, third.stderr);
            const info = await getSession(running.url, sid);
            assert.equal(info.session.message_count, 12);assert.equal(info.session.total_tokens, 12);
            assert.equal((await (await fetch(running.url + '/health')).json() as {loop_impl: string}).loop_impl, 'v2');
        } finally {await running.stop();}
    } finally {fs.rmSync(temp, {recursive: true, force: true});}
});
