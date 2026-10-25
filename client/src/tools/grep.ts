import * as fs from 'node:fs';
import * as path from 'node:path';
import {Worker} from 'node:worker_threads';
import {Handler, HandlerContext, ToolError, stringArg, MAX_FILE_BYTES, MAX_OUTPUT_BYTES, type ToolArgs} from './base.ts';

const SKIP = new Set(['.git', 'node_modules', '__pycache__', '.venv', 'dist', 'build']);

export class Grep extends Handler {
    name = 'grep';
    protected run(args: ToolArgs): Promise<string> {
        return new Promise((resolve, reject) => {
            const worker = new Worker(new URL('./grepWorker.ts', import.meta.url), {workerData: {workspace: this.ctx.workspace, args}});
            const signal = this.ctx.signal;
            let settled = false;
            const finish = (error?: Error, result?: string): void => {
                if (settled) return;
                settled = true;
                clearTimeout(timer);
                signal?.removeEventListener('abort', abort);
                void worker.terminate();
                if (error) reject(error); else resolve(result ?? '');
            };
            const abort = (): void => finish(new ToolError('search cancelled'));
            const timer = setTimeout(() => finish(new ToolError('search timed out after 5s')), 5000);
            signal?.addEventListener('abort', abort, {once: true});
            if (signal?.aborted) abort();
            worker.on('message', (message: {result?: string; error?: string}) => finish(message.error ? new ToolError(message.error) : undefined, message.result));
            worker.on('error', error => finish(error));
            worker.on('exit', () => { if (!settled) finish(new ToolError('search worker exited without a result')); });
        });
    }
}

// A worker makes even a pathological model-supplied regex cancellable.
export function search(ctx: HandlerContext, args: ToolArgs): string {
        const pattern = stringArg(args, 'pattern');
        if (pattern.length > 512) throw new ToolError('pattern exceeds 512 characters');
        let regex: RegExp;
        try { regex = new RegExp(pattern); }
        catch (e) { throw new ToolError(`invalid regex: ${(e as Error).message}`); }
        const root = ctx.resolve(stringArg(args, 'path', '.'));
        const hits: string[] = [];
        let entries = 0, bytes = 0, output = 0, skipped = 0, truncated = false;
        const visit = (file: string, depth: number): void => {
            if (truncated) return;
            if (++entries > 2000 || depth > 32) { truncated = true; return; }
            const safe = ctx.resolve(file);
            const stat = fs.lstatSync(safe);
            if (stat.isDirectory()) {
                const dir = fs.opendirSync(safe);
                try {
                    let entry: fs.Dirent | null;
                    while (!truncated && (entry = dir.readSync()) !== null) {
                        if (entry.isSymbolicLink() || SKIP.has(entry.name)) continue;
                        visit(path.join(safe, entry.name), depth + 1);
                    }
                } finally { dir.closeSync(); }
                return;
            }
            if (!stat.isFile() || stat.size > MAX_FILE_BYTES) { skipped++; return; }
            if (bytes + stat.size > 8 * MAX_FILE_BYTES) { truncated = true; return; }
            const content = ctx.readText(safe);
            bytes += Buffer.byteLength(content);
            if (content.includes('\0')) { skipped++; return; }
            const lines = content.split('\n');
            for (let i = 0; i < lines.length; i++) {
                if (lines[i].length > 4096) { skipped++; continue; }
                if (regex.test(lines[i])) {
                    const hit = `${path.relative(ctx.workspace, safe)}:${i + 1}:${lines[i]}`;
                    output += Buffer.byteLength(hit) + 1;
                    if (hits.length >= 200 || output > MAX_OUTPUT_BYTES - 128) { truncated = true; return; }
                    hits.push(hit);
                }
            }
        };
        visit(root, 0);
        if (truncated || skipped) hits.push(`... (search limited; skipped ${skipped} large/binary files or long lines${truncated ? '; budget reached' : ''})`);
        return hits.length ? hits.join('\n') : 'No matches.';
}
