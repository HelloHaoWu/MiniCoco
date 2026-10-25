import {spawn} from 'node:child_process';
import {Handler, ToolError, stringArg, MAX_OUTPUT_BYTES, type ToolArgs} from './base.ts';

// Commands are trusted local execution: cwd is not an OS sandbox.
export class RunCommand extends Handler {
    name = 'run_command';
    protected run(args: ToolArgs): Promise<string> {
        const command = stringArg(args, 'command');
        if (command.length > 16 * 1024) throw new ToolError('command exceeds 16384 characters');
        return new Promise((resolve, reject) => {
            const child = spawn(command, {cwd: this.ctx.workspace, shell: true, detached: process.platform !== 'win32', stdio: ['ignore', 'pipe', 'pipe']});
            const stdout: Buffer[] = [], stderr: Buffer[] = [];
            let bytes = 0, failure: Error | undefined;
            const stop = (reason: Error): void => {
                failure ??= reason;
                if (!child.pid) return;
                try {
                    if (process.platform === 'win32') child.kill('SIGKILL');
                    else process.kill(-child.pid, 'SIGKILL');
                } catch (e) { if ((e as NodeJS.ErrnoException).code !== 'ESRCH') failure = e as Error; }
            };
            const abort = (): void => stop(new ToolError('command cancelled'));
            const timer = setTimeout(() => stop(new ToolError('command timed out after 120s')), 120_000);
            const signal = this.ctx.signal;
            signal?.addEventListener('abort', abort, {once: true});
            if (signal?.aborted) abort();
            const collect = (target: Buffer[]) => (chunk: Buffer): void => {
                bytes += chunk.length;
                if (bytes > MAX_OUTPUT_BYTES) stop(new ToolError(`command output exceeds ${MAX_OUTPUT_BYTES} bytes`));
                else target.push(chunk);
            };
            child.stdout.on('data', collect(stdout));
            child.stderr.on('data', collect(stderr));
            child.on('error', error => { failure ??= error; });
            child.on('close', (code, exitSignal) => {
                clearTimeout(timer);
                signal?.removeEventListener('abort', abort);
                if (failure) { reject(failure); return; }
                resolve(`exit_code: ${code ?? 'null'}${exitSignal ? `\nsignal: ${exitSignal}` : ''}\nstdout:\n${Buffer.concat(stdout).toString('utf8').trimEnd()}\nstderr:\n${Buffer.concat(stderr).toString('utf8').trimEnd()}`);
            });
        });
    }
}
