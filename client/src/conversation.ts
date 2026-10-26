import {randomUUID} from 'node:crypto';
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
export interface ConversationOptions { mode?: ToolMode; sessionId?: string; onSession?: (id: string) => void; retryPolicy?: RetryPolicy; retryDependencies?: RetryDependencies; }

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
    private readonly mode: ToolMode;
    private options: ConversationOptions;
    private pending?: {request: RoundRequest; query: string};

    constructor(serverUrl: string, workspace: string, model = '', maxRounds = 25, options: ConversationOptions = {}) {
        const url = new URL(serverUrl);
        if (!['http:', 'https:'].includes(url.protocol)) throw new Error('server must use HTTP or HTTPS');
        if (!Number.isSafeInteger(maxRounds) || maxRounds < 1 || maxRounds > 100) throw new Error('maxRounds must be an integer between 1 and 100');
        if (options.sessionId && !/^[A-Za-z0-9_-]{1,128}$/.test(options.sessionId)) throw new Error('invalid session ID');
        this.serverUrl = serverUrl;
        this.ctx = new HandlerContext(workspace);
        this.workspace = this.ctx.workspace;
        this.model = model;
        this.maxRounds = maxRounds;
        this.options = options;
        this.mode = normalizeMode(options.mode);
        this.sessionId = options.sessionId ?? '';
    }

    private nextRequest(delta: {query: string} | {toolUseResults: ToolUseResult[]}): RoundRequest {
        return {sessionId: this.sessionId, roundId: `${this.sessionId || 'new'}-r-${randomUUID()}`, workspace: this.workspace, model: this.model, mode: this.mode, ...delta};
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
            this.pending ??= {request: this.nextRequest({query}), query};
            for (rounds = 1; rounds <= this.maxRounds; rounds++) {
                signal?.throwIfAborted();
                process.stdout.write(`\n--- round ${rounds} ---\n`);
                const result = await runRoundWithRetry({...this.pending.request, serverUrl: this.serverUrl, options: {onSession: id => {
                    if (this.sessionId !== id) {this.sessionId = id;this.options.onSession?.(id);}
                }}}, this.options.retryPolicy ?? RETRY_POLICY, signal, this.options.retryDependencies);
                this.sessionId = result.sessionId ?? this.sessionId;
                if (!this.sessionId) throw new Error('backend did not identify the session');
                // Check the whole batch before any local side effect, including
                // injected transports that bypass the normal event consumer.
                for (const call of result.toolCalls) {
                    if (!toolAllowed(this.mode, call.name)) throw new RoundError(`tool ${call.name} is not allowed in mode ${this.mode}`, 'protocol');
                }
                if (result.finish === FINISH.STOP) {
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
            const message = error instanceof RoundError ? `${error.code ?? error.kind}: ${error.message} after ${error.attempts ?? 1} tries${error.traceId ? ` (trace ${error.traceId})` : ''}` : (error as Error).message;
            return {status: signal?.aborted ? 'cancelled' : 'failed', text: '', rounds, sessionId: this.sessionId || undefined, message};
        } finally {this.ctx.signal = undefined;this.running = false;}
    }

    async runTool(call: ToolCall): Promise<string> {
        process.stdout.write(`→ ${call.name} ${JSON.stringify(call.arguments).slice(0, 120)}\n`);
        let result: string;
        try {
            const handler = createHandler(call.name, this.ctx);
            if (!handler) throw new ToolError(`unknown tool: ${call.name}`);
            if (!toolAllowed(this.mode, call.name)) throw new ToolError(`tool ${call.name} is not allowed in mode ${this.mode}`);
            result = await handler.execute(call.arguments);
        } catch (error) {
            if (this.ctx.signal?.aborted) throw error;
            result = `ERROR: ${(error as Error).message}`;
        }
        result = boundedOutput(result);
        process.stdout.write(result.slice(0, 240) + (result.length > 240 ? '...\n' : '\n'));
        return result;
    }
}
