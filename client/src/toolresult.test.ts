import {test} from 'node:test';
import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import * as http from 'node:http';
import {once} from 'node:events';
import {spawn, spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {getSession} from './sessions.ts';

const project = fileURLToPath(new URL('../..', import.meta.url));

async function cli(args: string[]): Promise<{code: number | null; stdout: string; stderr: string}> {
    const child = spawn(process.execPath, [path.join(project, 'client/src/cli.ts'), ...args], {stdio: ['pipe', 'pipe', 'pipe']});
    let stdout = '', stderr = '';
    child.stdout.on('data', chunk => {stdout += chunk;});child.stderr.on('data', chunk => {stderr += chunk;});
    const closed = once(child, 'close');child.stdin.end();const [code] = await closed;
    return {code, stdout, stderr};
}

async function backend(binary: string, dir: string, native: boolean): Promise<{url: string; stop: () => Promise<void>}> {
    const reservation = http.createServer();reservation.listen(0, '127.0.0.1');await once(reservation, 'listening');
    const port = (reservation.address() as import('node:net').AddressInfo).port;
    await new Promise<void>(resolve => reservation.close(() => resolve()));
    const child = spawn(binary, [], {cwd: dir, env: {...process.env, STORE_BACKEND: 'file', MINICOCO_DATA_DIR: path.join(dir, 'data'),
        MINICOCO_MODEL_IS_FUNCTION_CALL: String(native), MINICOCO_DEBUG_PROMPT: '0', LLM_PROVIDER: 'mock', LLM_API_KEY: '', LLM_MOCK_MODE: 'normal',
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

test('real Go backend and CLI persist/resume both Native and XML tool results', {timeout: 30_000}, async () => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-format-cli-'));
    try {
        const binary = path.join(temp, 'backend');
        const build = spawnSync('go', ['build', '-o', binary, '.'], {cwd: path.join(project, 'backend'), encoding: 'utf8'});
        assert.equal(build.status, 0, build.stderr);
        for (const native of [true, false]) {
            const dir = path.join(temp, String(native));fs.mkdirSync(dir);
            const workspace = path.join(dir, 'workspace');fs.mkdirSync(workspace);
            let running = await backend(binary, dir, native);
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
                assert.equal((await getSession(running.url, sid)).session.message_count, 4);
                await running.stop();running = await backend(binary, dir, native);
                const resumed = await cli(['again', '--server', running.url, '--resume', sid, '--mode', 'readonly']);
                assert.equal(resumed.code, 0, resumed.stderr);
                assert.equal((await getSession(running.url, sid)).session.message_count, 8);
                const listed = await cli(['--list', '--server', running.url]);assert.equal(listed.code, 0, listed.stderr);assert.match(listed.stdout, new RegExp(sid));
            } finally {await running.stop();}
        }
    } finally {fs.rmSync(temp, {recursive: true, force: true});}
});
