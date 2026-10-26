import * as fs from 'node:fs';
import * as path from 'node:path';
import {randomUUID} from 'node:crypto';
import type {BackgroundTasks} from '../background.ts';

export const MAX_FILE_BYTES = 1024 * 1024;
export const MAX_OUTPUT_BYTES = 64 * 1024;
export class ToolError extends Error {}
export type ToolArgs = Record<string, unknown>;

export function stringArg(args: ToolArgs, key: string, fallback?: string, allowEmpty = false): string {
    const value = args[key] === undefined ? fallback : args[key];
    if (typeof value !== 'string' || (!allowEmpty && value.length === 0) || value.includes('\0')) {
        throw new ToolError(`${key} must be ${allowEmpty ? 'a' : 'a non-empty'} string without NUL bytes`);
    }
    return value;
}

export function integerArg(args: ToolArgs, key: string, fallback: number, max: number): number {
    const value = args[key] === undefined ? fallback : args[key];
    if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 1 || value > max) {
        throw new ToolError(`${key} must be an integer between 1 and ${max}`);
    }
    return value;
}

// Match Go's JSON encoding, including HTML and Unicode line-separator escapes,
// so the server's reservation covers the bytes stored and sent upstream.
function encodedOutputBytes(text: string): number {
    return Buffer.byteLength(JSON.stringify(text).replace(/[<>&\u2028\u2029]/g, character => '\\u' + character.charCodeAt(0).toString(16).padStart(4, '0')));
}

export function boundedOutput(text: string): string {
    const bytes = Buffer.from(text);
    if (bytes.length <= MAX_OUTPUT_BYTES && encodedOutputBytes(text) <= MAX_OUTPUT_BYTES) return text;
    const suffix = '\n... (output truncated)';
    let low = 0, high = Math.min(text.length, MAX_OUTPUT_BYTES);
    while (low < high) {
        const mid = Math.ceil((low + high) / 2);
        if (encodedOutputBytes(text.slice(0, mid) + suffix) <= MAX_OUTPUT_BYTES) low = mid;
        else high = mid - 1;
    }
    return text.slice(0, low).replace(/[\uD800-\uDBFF]$/, '') + suffix;
}

export class HandlerContext {
    workspace: string;
    signal?: AbortSignal;
    delegate?: (args: ToolArgs) => Promise<string>;
    background?: BackgroundTasks;

    constructor(workspace: string) {
        this.workspace = fs.realpathSync(workspace);
        if (!fs.statSync(this.workspace).isDirectory()) throw new ToolError('workspace must be a directory');
    }

    // File-tool guard, not an OS sandbox. Reject links, including new-file parents.
    resolve(p = '.'): string {
        if (typeof p !== 'string' || p.includes('\0')) throw new ToolError('path must be a string without NUL bytes');
        const candidate = path.resolve(this.workspace, p);
        const relative = path.relative(this.workspace, candidate);
        if (relative === '..' || relative.startsWith('..' + path.sep) || path.isAbsolute(relative)) {
            throw new ToolError(`path escapes workspace root: ${p}`);
        }
        let current = this.workspace;
        for (const part of relative.split(path.sep).filter(Boolean)) {
            current = path.join(current, part);
            try {
                if (fs.lstatSync(current).isSymbolicLink()) throw new ToolError(`symlinks are not allowed: ${p}`);
            } catch (e) {
                if ((e as NodeJS.ErrnoException).code === 'ENOENT') break;
                throw e;
            }
        }
        return candidate;
    }

    readText(file: string): string {
        const fd = fs.openSync(file, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
        try {
            const stat = fs.fstatSync(fd);
            if (!stat.isFile()) throw new ToolError('target must be a regular file');
            if (stat.size > MAX_FILE_BYTES) throw new ToolError(`file exceeds ${MAX_FILE_BYTES} bytes`);
            const buffer = Buffer.alloc(MAX_FILE_BYTES + 1);
            let size = 0;
            while (size < buffer.length) {
                const n = fs.readSync(fd, buffer, size, buffer.length - size, null);
                if (n === 0) break;
                size += n;
            }
            if (size > MAX_FILE_BYTES) throw new ToolError(`file exceeds ${MAX_FILE_BYTES} bytes`);
            return buffer.toString('utf8', 0, size);
        } finally { fs.closeSync(fd); }
    }

    writeText(file: string, content: string): void {
        if (Buffer.byteLength(content) > MAX_FILE_BYTES) throw new ToolError(`content exceeds ${MAX_FILE_BYTES} bytes`);
        let mode = 0o644;
        try {
            const stat = fs.lstatSync(file);
            if (!stat.isFile()) throw new ToolError('target must be a regular file');
            mode = stat.mode & 0o777;
        } catch (e) {
            if ((e as NodeJS.ErrnoException).code !== 'ENOENT') throw e;
        }
        fs.mkdirSync(path.dirname(file), {recursive: true});
        this.resolve(file);
        const temporary = path.join(path.dirname(file), `.minicoco-${randomUUID()}.tmp`);
        try {
            fs.writeFileSync(temporary, content, {flag: 'wx', mode});
            fs.renameSync(temporary, file);
        } finally { fs.rmSync(temporary, {force: true}); }
    }
}

export abstract class Handler {
    abstract name: string;
    requiresApproval = false;
    ctx: HandlerContext;
    constructor(ctx: HandlerContext) { this.ctx = ctx; }

    execute(args: ToolArgs): Promise<string> | string {
        this.ctx.signal?.throwIfAborted();
        if (!args || typeof args !== 'object' || Array.isArray(args)) throw new ToolError('arguments must be an object');
        const normalize = (error: unknown): never => {
            if (this.ctx.signal?.aborted || error instanceof ToolError) throw error;
            throw new ToolError((error as Error).message);
        };
        try {
            const result = this.run(args);
            return typeof result === 'string' ? boundedOutput(result) : result.then(boundedOutput).catch(normalize);
        } catch (error) { return normalize(error); }
    }
    protected abstract run(args: ToolArgs): Promise<string> | string;
}
