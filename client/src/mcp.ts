import {spawn, type ChildProcessWithoutNullStreams} from 'node:child_process';
import * as fs from 'node:fs';
import {Handler, ToolError, type HandlerContext, type ToolArgs} from './tools/base.ts';
import {MCP_TIMEOUT_MS} from './tuning.ts';
import type {MCPToolSpec} from './protocol.ts';

interface ServerConfig {name: string; transport: 'stdio'; command: string; args: string[]; readOnly: boolean;}
const object = (value: unknown): value is Record<string, unknown> => !!value && typeof value === 'object' && !Array.isArray(value);
const nameOK = (s: unknown): s is string => typeof s === 'string' && /^[A-Za-z0-9_-]{1,24}$/.test(s) && !s.includes('__');

// One bounded, newline-delimited JSON-RPC connection. Never retries tool calls.
export class StdioMcp {
    private proc: ChildProcessWithoutNullStreams;
    private nextID = 1;
    private pending = new Map<number, {finish: (error?: Error, result?: unknown) => void}>();
    private input = Buffer.alloc(0);
    private dead = false;
    private timeout: number;
    private rateStart = Date.now();
    private rateBytes = 0;
    private closed: Promise<void>;
    private closing?: Promise<void>;
    constructor(command: string, args: string[], workspace: string, timeout = MCP_TIMEOUT_MS) {
        this.timeout = timeout;
        this.proc = spawn(command, args, {cwd: workspace, stdio: ['pipe', 'pipe', 'pipe'], detached: process.platform !== 'win32'});
        this.closed = new Promise(resolve => this.proc.once('close', () => {this.fail(new Error('mcp server unavailable'));resolve();}));
        this.proc.on('error', () => this.fail(new Error('mcp server unavailable')));
        this.proc.stdin.on('error', () => this.fail(new Error('mcp server unavailable')));
        this.proc.stderr.resume(); // Do not copy external process output or secrets into diagnostics.
        this.proc.stdout.on('data', (chunk: Buffer) => {
            if (Date.now()-this.rateStart>1000) {this.rateStart=Date.now();this.rateBytes=0;}
            this.rateBytes+=chunk.length;if(this.rateBytes>16*1024*1024) {void this.close();return;}
            this.input = Buffer.concat([this.input, chunk]);
            let end: number;
            while ((end = this.input.indexOf(10)) !== -1) {
                if (end > 1024 * 1024) {void this.close();return;}
                const line = this.input.subarray(0, end);this.input = this.input.subarray(end + 1);
                let message: unknown;try {message = JSON.parse(line.toString('utf8'));} catch {continue;}
                if (!object(message) || message.jsonrpc !== '2.0') continue;
                if (typeof message.method === 'string') {
                    if (message.id !== undefined) this.write({jsonrpc: '2.0', id: message.id, error: {code: -32601, message: 'Method not supported'}});
                    continue;
                }
                if (typeof message.id !== 'number') continue;
                const p = this.pending.get(message.id);
                if (p) p.finish(message.error ? new Error('mcp RPC error') : undefined, message.result);
            }
            if (this.input.length > 1024 * 1024) void this.close();
        });
    }
    private fail(error: Error): void {this.dead = true;for (const p of this.pending.values()) p.finish(error);}
    private write(value: unknown): void {
        if (!this.dead) this.proc.stdin.write(JSON.stringify(value) + '\n');
    }
    notify(method: string, params?: unknown): void {this.write({jsonrpc: '2.0', method, params});}
    rpc(method: string, params?: unknown, signal?: AbortSignal): Promise<unknown> {
        if (signal?.aborted) return Promise.reject(signal.reason);
        if (Buffer.byteLength(JSON.stringify(params) ?? '')>1024*1024) return Promise.reject(new Error('mcp request exceeds limit'));
        if (this.dead || this.pending.size >= 16) return Promise.reject(new Error('mcp server unavailable or busy'));
        return new Promise((resolve, reject) => {
            const id = this.nextID++;
            const finish = (error?: Error, result?: unknown): void => {
                if (!this.pending.delete(id)) return;
                clearTimeout(timer);signal?.removeEventListener('abort', abort);
                if (error) reject(error);else resolve(result);
            };
            const cancel = (error: Error): void => {
                if (method === 'initialize') {finish(error);void this.close();}
                else {this.notify('notifications/cancelled', {requestId: id, reason: 'Request cancelled'});finish(error);}
            };
            const abort = (): void => cancel(new Error('mcp request cancelled'));
            const timer = setTimeout(() => cancel(new Error('mcp timeout')), this.timeout);
            this.pending.set(id, {finish}); // Register before write: fast responses cannot be lost.
            signal?.addEventListener('abort', abort, {once: true});
            this.write({jsonrpc: '2.0', id, method, params});
        });
    }
    close(): Promise<void> {
        return this.closing ??= (async () => {
            this.fail(new Error('mcp server unavailable'));this.proc.stdin.end();
            const kill = (signal: NodeJS.Signals): void => {try {if (this.proc.pid && process.platform !== 'win32') process.kill(-this.proc.pid, signal);else this.proc.kill(signal);} catch {}};
            const term = setTimeout(() => kill('SIGTERM'), 100), hard = setTimeout(() => kill('SIGKILL'), 350);
            try {await this.closed;} finally {clearTimeout(term);clearTimeout(hard);kill('SIGKILL');}
        })();
    }
}

export class MCPManager {
    private servers = new Map<string, StdioMcp>();
    private entries = new Map<string, {server: StdioMcp; tool: string; spec: MCPToolSpec}>();
    catalog(): MCPToolSpec[] {return structuredClone([...this.entries.values()].map(v => v.spec).sort((a,b) => a.name.localeCompare(b.name)));}
    static async connect(configPath: string, workspace: string, signal?: AbortSignal): Promise<MCPManager> {
        const manager = new MCPManager();
        const fd = fs.openSync(configPath, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
        let data: unknown;
        try {if (!fs.fstatSync(fd).isFile() || fs.fstatSync(fd).size > 65536) throw new Error('invalid MCP config size');const buf=Buffer.alloc(65537);const count=fs.readSync(fd,buf,0,buf.length,0);if(count>65536)throw new Error('MCP config exceeds 64 KiB');data=JSON.parse(buf.toString('utf8',0,count));} finally {fs.closeSync(fd);}
        if (!object(data) || !Array.isArray(data.servers) || data.servers.length > 8) throw new Error('MCP config requires at most 8 servers');
        const seen = new Set<string>();
        const configs: ServerConfig[] = data.servers.map(v => {
            if (!object(v) || !nameOK(v.name) || seen.has(v.name) || v.transport !== 'stdio' || typeof v.command !== 'string' || !v.command || v.command.includes('\0') || (v.args !== undefined && (!Array.isArray(v.args) || v.args.length > 64 || !v.args.every(a => typeof a === 'string' && !a.includes('\0')))) || (v.readOnly !== undefined && typeof v.readOnly !== 'boolean')) throw new Error('invalid MCP server config (stdio only)');
            seen.add(v.name);return {name:v.name,transport:'stdio',command:v.command,args:(v.args ?? []) as string[],readOnly:v.readOnly === true};
        });
        try {
            for (const cfg of configs) {
                signal?.throwIfAborted();
                const server = new StdioMcp(cfg.command,cfg.args,workspace);
                try {
                    const init=await server.rpc('initialize',{protocolVersion:'2024-11-05',capabilities:{},clientInfo:{name:'minicoco',version:'0'}},signal);
                    if (!object(init) || init.protocolVersion !== '2024-11-05' || !object(init.capabilities) || !object(init.capabilities.tools)) throw new Error('unsupported MCP initialization');
                    server.notify('notifications/initialized');
                    let cursor: string | undefined;const discovered: Array<{tool:string;spec:MCPToolSpec}> = [];
                    for (let page=0;page<8;page++) {
                        const list=await server.rpc('tools/list',cursor ? {cursor} : {},signal);
                        if (!object(list) || !Array.isArray(list.tools)) throw new Error('invalid MCP tools/list');
                        for (const tool of list.tools) {
                            if (!object(tool) || !nameOK(tool.name) || (tool.description !== undefined && typeof tool.description !== 'string') || !object(tool.inputSchema) || tool.inputSchema.type !== 'object') throw new Error('invalid MCP tool');
                            const name=`mcp__${cfg.name}__${tool.name}`;
                            if (discovered.some(t=>t.spec.name===name)) throw new Error('duplicate MCP tool');
                            discovered.push({tool:tool.name,spec:{name,description:(tool.description ?? '') as string,parameters:tool.inputSchema,readOnly:cfg.readOnly}});
                        }
                        if (discovered.length + manager.entries.size > 64 || Buffer.byteLength(JSON.stringify([...manager.catalog(),...discovered.map(t=>t.spec)])) > 65536) throw new Error('MCP catalog exceeds budget');
                        if (list.nextCursor === undefined) {cursor=undefined;break;}
                        if (typeof list.nextCursor !== 'string' || !list.nextCursor) throw new Error('invalid MCP cursor');
                        cursor=list.nextCursor;
                    }
                    if (cursor) throw new Error('too many MCP pages');
                    manager.servers.set(cfg.name,server);
                    for (const entry of discovered) manager.entries.set(entry.spec.name,{server,...entry});
                } catch {
                    await server.close();process.stderr.write(`[mcp] server ${cfg.name} unavailable; skipped\n`);signal?.throwIfAborted();
                }
            }
            return manager;
        } catch (error) {await manager.close();throw error;}
    }
    handler(name: string, ctx: HandlerContext): Handler | null {
        const entry=this.entries.get(name);if (!entry) return null;
        return new McpHandler(ctx,name,entry.server,entry.tool,entry.spec.readOnly);
    }
    async close(): Promise<void> {await Promise.all([...this.servers.values()].map(s=>s.close()));this.servers.clear();}
}
class McpHandler extends Handler {
    name: string;
    private server: StdioMcp;
    private tool: string;
    constructor(ctx: HandlerContext,name:string,server:StdioMcp,tool:string,readOnly:boolean) {super(ctx);this.name=name;this.server=server;this.tool=tool;this.requiresApproval=!readOnly;}
    protected async run(args: ToolArgs): Promise<string> {
        let res: unknown;
        try {res=await this.server.rpc('tools/call',{name:this.tool,arguments:args},this.ctx.signal);} catch {throw new ToolError(`mcp server ${this.name.split('__')[1]} unavailable`);}
        if (!object(res) || !Array.isArray(res.content)) throw new ToolError('invalid MCP tool result');
        const text=res.content.map(c=>object(c) && c.type==='text' && typeof c.text==='string' ? c.text : JSON.stringify(c)).join('\n');
        if(res.isError) throw new ToolError(text || 'mcp tool error');return text;
    }
}
