import {spawn, execFile} from 'node:child_process';
import {randomUUID} from 'node:crypto';
import {mkdirSync, writeFileSync} from 'node:fs';
import {dirname} from 'node:path';
import {Handler, ToolError, stringArg, type ToolArgs, type HandlerContext} from './base.ts';
import {commandLimits} from '../tuning.ts';

// Resource guards bound accidental runaway work; they are not an OS sandbox.
export function executeCommand(ctx: HandlerContext, args: ToolArgs): Promise<string> {
    const command = stringArg(args, 'command');
    if (command.length > 16 * 1024) throw new ToolError('command exceeds 16384 characters');
    if (process.platform === 'win32') throw new ToolError('guarded commands currently require macOS or Linux');
    const limits = commandLimits();
    ctx.signal?.throwIfAborted();
    return new Promise((resolve, reject) => {
        const child = spawn(command, {cwd: ctx.workspace, shell: true, detached: true, stdio: ['ignore', 'pipe', 'pipe']});
        const stdout: Buffer[] = [], stderr: Buffer[] = [], combined: Buffer[] = [];
        let bytes = 0, tail = Buffer.alloc(0), failure: Error | undefined, overflow = false, closed = false, polling = false;
        let killTimer: ReturnType<typeof setTimeout> | undefined;
        const kill = (signal: NodeJS.Signals): void => {
            if (!child.pid) return;
            try {process.kill(-child.pid, signal);} catch (error) {if ((error as NodeJS.ErrnoException).code !== 'ESRCH') failure ??= error as Error;}
        };
        const stop = (reason: Error): void => {
            if (failure || closed) return;
            failure = reason;kill('SIGTERM');killTimer = setTimeout(() => kill('SIGKILL'), 250);
        };
        const abort = (): void => stop(new ToolError('command cancelled'));
        const timer = setTimeout(() => stop(new ToolError(`command timed out after ${limits.timeoutMs}ms`)), limits.timeoutMs);
        const signal = ctx.signal;
        signal?.addEventListener('abort', abort, {once: true});if (signal?.aborted) abort();
        const monitor = setInterval(() => {
            if (polling || closed || failure || !child.pid) return;
            polling = true;
            // Sum the process group, including ordinary shell descendants.
            execFile('ps', ['-axo', 'pid=,pgid=,rss='], {timeout: 1000, maxBuffer: 1024 * 1024}, (error, output) => {
                polling = false;if (closed || failure) return;
                if (error) {stop(new ToolError('cannot monitor command memory'));return;}
                let rss = 0;
                for (const line of output.trim().split('\n')) {const fields = line.trim().split(/\s+/).map(Number);if (fields.length === 3 && fields[1] === child.pid) rss += fields[2] * 1024;}
                if (rss > limits.rssBytes) stop(new ToolError('killed (memory)'));
            });
        }, 100);
        const collect = (target: Buffer[]) => (chunk: Buffer): void => {
            if (overflow || closed) return;
            const allowed = chunk.subarray(0, Math.max(0, limits.outputBytes - bytes));
            if (allowed.length) {target.push(allowed);combined.push(allowed);bytes += allowed.length;}
            tail = Buffer.concat([tail, chunk.subarray(-2048)]).subarray(-2048);
            if (allowed.length < chunk.length) {overflow = true;stop(new ToolError('killed (output limit)'));}
        };
        child.stdout.on('data', collect(stdout));child.stderr.on('data', collect(stderr));
        child.on('error', error => {failure ??= error;});
        child.on('close', (code, exitSignal) => {
            closed = true;clearTimeout(timer);clearInterval(monitor);if (killTimer) clearTimeout(killTimer);
            signal?.removeEventListener('abort', abort);
            if (failure) kill('SIGKILL');
            if (overflow && !signal?.aborted) {
                const relative = `data/terminals/${randomUUID()}.out`;
                try {
                    const file = ctx.resolve(relative);mkdirSync(dirname(file), {recursive: true});ctx.resolve(relative);
                    writeFileSync(file, Buffer.concat(combined), {flag: 'wx', mode: 0o600});
                    resolve(`ERROR: killed (output limit); 已截断，已捕获输出见 ${relative} (最多 ${limits.outputBytes} bytes，后续输出未保存)\n${tail.toString('utf8')}`);
                } catch (error) {reject(new ToolError(`output truncated; could not save log: ${(error as Error).message}`));}
                return;
            }
            if (failure) {reject(failure);return;}
            resolve(`exit_code: ${code ?? 'null'}${exitSignal ? `\nsignal: ${exitSignal}` : ''}\nstdout:\n${Buffer.concat(stdout).toString('utf8').trimEnd()}\nstderr:\n${Buffer.concat(stderr).toString('utf8').trimEnd()}`);
        });
    });
}

export class RunCommand extends Handler {
    requiresApproval = true;
    name = 'run_command';
    protected run(args: ToolArgs): Promise<string> {return executeCommand(this.ctx, args);}
}
