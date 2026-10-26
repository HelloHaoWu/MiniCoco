import {readMemoryIndex} from './memory.ts';
import {sceneFor, sceneMode, parseSceneQuery} from './scenes.ts';
import {SUBAGENTS, SUB_MAX_ROUNDS} from './agents.ts';
import {BackgroundTasks} from './background.ts';
import {randomUUID, createHash} from 'node:crypto';
import {approve, type ApprovalMode, type ApprovalPrompt} from './approval.ts';
import {AuditClient} from './audit.ts';
import {runRoundWithRetry, type RetryDependencies} from './retry.ts';
import {RETRY_POLICY, type RetryPolicy} from './tuning.ts';
import {RoundError} from './roundError.ts';
import {createHandler, HandlerContext, ToolError} from './tools/index.ts';
import {boundedOutput} from './tools/base.ts';
import {FINISH, type RoundRequest, type ToolCall, type ToolUseResult, type ToolMode} from './protocol.ts';
import {normalizeMode, toolAllowed} from './toolMode.ts';

export const MAX_HISTORY_BYTES = 1024 * 1024;
export interface RunResult {
    status: 'completed' | 'failed' | 'exhausted' | 'cancelled';
    text: string;
    rounds: number;
    sessionId?: string;
    message?: string;
}
export interface ConversationOptions { mcp?: import('./mcp.ts').MCPManager; mode?: ToolMode; sessionId?: string; onSession?: (id: string) => void; retryPolicy?: RetryPolicy; retryDependencies?: RetryDependencies; approvalMode?: ApprovalMode; approvalPrompt?: ApprovalPrompt; agentType?: string; parentSessionId?: string; quiet?: boolean; waitBackground?: boolean; slash?: string; }

// Only session identity and an unacknowledged delta are kept here. The server
// owns full history, budgets, input reservations and successful-round commits.
export class Conversation {
    serverUrl: string;
    workspace: string;
    model: string;
    maxRounds: number;
    ctx: HandlerContext;
    sessionId: string;
    private running = false;
    private readonly baseMode: ToolMode;
    private mode: ToolMode;
    private slash: string;
    private options: ConversationOptions;
    private pending?: {request: RoundRequest; query: string};
    private audit: AuditClient;
    private background = new BackgroundTasks();
    private traceId = '';

    constructor(serverUrl: string, workspace: string, model = '', maxRounds = 25, options: ConversationOptions = {}) {
        const url = new URL(serverUrl);
        if (!['http:', 'https:'].includes(url.protocol)) throw new Error('server must use HTTP or HTTPS');
        if (!Number.isSafeInteger(maxRounds) || maxRounds < 1 || maxRounds > 100) throw new Error('maxRounds must be an integer between 1 and 100');
        if (options.sessionId && !/^[A-Za-z0-9_-]{1,128}$/.test(options.sessionId)) throw new Error('invalid session ID');
        this.serverUrl = serverUrl;
        this.audit = new AuditClient(serverUrl);
        this.ctx = new HandlerContext(workspace);
        this.ctx.background = this.background;
        this.ctx.mcp = options.mcp;
        this.workspace = this.ctx.workspace;
        this.model = model;
        this.maxRounds = maxRounds;
        this.options = options;
        this.baseMode = normalizeMode(options.mode);
        const scene = sceneFor(options.slash);this.slash = scene === 'composer' ? '' : scene;
        this.mode = options.agentType ? 'readonly' : sceneMode(this.baseMode, scene);
        this.sessionId = options.sessionId ?? '';
        this.ctx.delegate = async args => {
            if (this.options.agentType || !this.sessionId) throw new ToolError('subagent depth/session denied');
            if (args.agent_type !== 'explore') throw new ToolError('unknown agent_type');
            const spec = SUBAGENTS.explore;
            const sub = new Conversation(this.serverUrl, this.workspace, this.model.startsWith('mock') ? this.model : spec.model, SUB_MAX_ROUNDS, {mode: spec.mode, approvalMode: 'deny', agentType: 'explore', parentSessionId: this.sessionId, quiet: true});
            try {const result = await sub.run(String(args.query), this.ctx.signal);return result.status === 'completed' ? result.text : 'ERROR: 子任务未完成（' + result.status + '）: ' + (result.message ?? '');}
            finally {await sub.close();}
        };
    }

    private nextRequest(delta: {query: string} | {toolUseResults: ToolUseResult[]}): RoundRequest {
        let memoryIndex: string | undefined;
        try {memoryIndex=readMemoryIndex(this.ctx) || undefined;} catch {process.stderr.write('[memory] invalid or unreadable index; omitted from this round\n');}
        const completed = this.background.takeCompleted();
        return {memoryIndex, mcpTools: this.options.mcp?.catalog(), sessionId: this.sessionId, roundId: `${this.sessionId || 'new'}-r-${randomUUID()}`, workspace: this.workspace, model: this.model, mode: this.mode, slash: this.slash || undefined, agentType: this.options.agentType, parentSessionId: this.options.parentSessionId, backgroundResults: completed.length ? completed : undefined, ...delta};
    }

    async run(query: string, signal?: AbortSignal): Promise<RunResult> {
        if (this.running) return {status: 'failed', text: '', rounds: 0, message: 'conversation is already running'};
        this.running = true;
        this.ctx.signal = signal;
        let rounds = 0;
        try {
            signal?.throwIfAborted();
            if (typeof query !== 'string' || !query.trim()) throw new Error('query must be a non-empty string');
            if (Buffer.byteLength(JSON.stringify({role: 'user', content: query})) > MAX_HISTORY_BYTES) throw new Error('query exceeds conversation budget (1 MiB)');
            if (this.pending && this.pending.query !== query) throw new Error('an unacknowledged round remains; retry the same query or start a new conversation');
            const selected = parseSceneQuery(query, this.options.slash);
            if (this.pending && sceneFor(this.pending.request.slash) !== selected.scene) throw new Error('an unacknowledged round cannot change scene');
            this.slash = selected.scene === 'composer' ? '' : selected.scene;
            this.mode = this.options.agentType ? 'readonly' : sceneMode(this.baseMode, selected.scene);
            this.pending ??= {request: this.nextRequest({query: selected.query}), query};
            if (!this.options.quiet) process.stdout.write(`[scene ${selected.scene}] mode=${this.mode}\n`);
            for (rounds = 1; rounds <= this.maxRounds; rounds++) {
                signal?.throwIfAborted();
                if (!this.options.quiet) process.stdout.write(`\n--- round ${rounds} ---\n`);
                const result = await runRoundWithRetry({...this.pending.request, serverUrl: this.serverUrl, options: {onEvent: this.options.quiet ? () => {} : undefined, onSession: id => {
                    if (this.sessionId !== id) {this.sessionId = id;this.options.onSession?.(id);}
                }}}, this.options.retryPolicy ?? RETRY_POLICY, signal, this.options.retryDependencies);
                this.sessionId = result.sessionId ?? this.sessionId;
                this.traceId = result.traceId ?? '';
                if (!this.sessionId) throw new Error('backend did not identify the session');
                // Check the whole batch before any local side effect, including
                // injected transports that bypass the normal event consumer.
                for (const call of result.toolCalls) {
                    if (!toolAllowed(this.mode, call.name, this.options.mcp?.catalog())) throw new RoundError(`tool ${call.name} is not allowed in mode ${this.mode}`, 'protocol');
                }
                if (result.finish === FINISH.STOP) {
                    if (this.options.waitBackground) await this.background.wait(signal);
                    signal?.throwIfAborted();
                    if (this.background.hasCompleted) {
                        this.pending = {request: this.nextRequest({query: 'Process the completed background tasks.'}), query};
                        if (rounds === this.maxRounds) return {status:'exhausted',text:'',rounds,sessionId:this.sessionId,message:'background results retained; round budget reached'};
                        continue;
                    }
                    this.pending = undefined;
                    return {status: 'completed', text: result.assistant.content ?? '', rounds, sessionId: this.sessionId};
                }
                if (rounds === this.maxRounds) return {status: 'exhausted', text: '', rounds, sessionId: this.sessionId, message: `reached maxRounds=${this.maxRounds}; last tool batch was not executed`};
                const replies: ToolUseResult[] = [];
                for (const call of result.toolCalls) {
                    let output: string;
                    if (signal?.aborted) output = 'ERROR: cancelled; tool was not executed';
                    else {
                        try {output = await this.runTool(call);}
                        catch (error) {
                            if (!signal?.aborted) throw error;
                            output = 'ERROR: cancelled; an executing command may have made partial changes';
                        }
                    }
                    replies.push({id: call.id, name: call.name, output, isError: output.startsWith('ERROR:')});
                }
                // Keep results even after cancellation/failure, so an in-process
                // retry submits them instead of executing the same tools again.
                this.pending = {request: this.nextRequest({toolUseResults: replies}), query};
                signal?.throwIfAborted();
            }
            throw new Error('unreachable round state');
        } catch (error) {
            if (signal?.aborted) this.background.cancel();
            const message = error instanceof RoundError ? `${error.code ?? error.kind}: ${error.message} after ${error.attempts ?? 1} tries${error.traceId ? ` (trace ${error.traceId})` : ''}` : (error as Error).message;
            return {status: signal?.aborted ? 'cancelled' : 'failed', text: '', rounds, sessionId: this.sessionId || undefined, message};
        } finally {this.ctx.signal = undefined;this.running = false;}
    }

    async runTool(call: ToolCall): Promise<string> {
        if (!this.options.quiet) process.stdout.write(`→ ${call.name} ${JSON.stringify(call.arguments).slice(0, 120)}\n`);
        let result: string;
        const start = performance.now();
        let approved = false, status: 'ok' | 'error' | 'denied' = 'error';
        try {
            const handler = createHandler(call.name, this.ctx);
            if (!handler) throw new ToolError(`unknown tool: ${call.name}`);
            if (!toolAllowed(this.mode, call.name, this.options.mcp?.catalog())) throw new ToolError(`tool ${call.name} is not allowed in mode ${this.mode}`);
            approved = await approve(handler, call, this.options.approvalMode ?? 'deny', this.options.approvalPrompt);
            if (!approved) {status = 'denied';result = 'ERROR: denied by user';}
            else {result = await handler.execute(call.arguments);status = result.startsWith('ERROR:') ? 'error' : 'ok';}
        } catch (error) {
            if (this.ctx.signal?.aborted) throw error;
            result = `ERROR: ${(error as Error).message}`;
        } finally {
            this.audit.emit({workspaceDigest: createHash('sha256').update(this.workspace).digest('hex').slice(0,12), ts: Date.now(), sessionId: this.sessionId, traceId: this.traceId, tool: call.name, argsDigest: createHash('sha1').update(JSON.stringify(call.arguments)).digest('hex').slice(0, 12), approved, status, ms: Math.max(0, Math.round(performance.now() - start))});
        }
        result = boundedOutput(result);
        if (!this.options.quiet) process.stdout.write(result.slice(0, 240) + (result.length > 240 ? '...\n' : '\n'));
        return result;
    }

    setMCP(mcp: import('./mcp.ts').MCPManager): void {this.options.mcp=mcp;this.ctx.mcp=mcp;}

    async close(): Promise<void> {await this.background.close();await this.options.mcp?.close();await this.audit.flush();}
}
