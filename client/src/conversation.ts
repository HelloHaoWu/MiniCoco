import {runRound} from './executor.ts';
import {createHandler, HandlerContext, ToolError} from './tools/index.ts';
import {boundedOutput} from './tools/base.ts';
import {FINISH, type Message, type ToolCall} from './protocol.ts';

export const MAX_HISTORY_BYTES = 1024 * 1024;
export const MAX_HISTORY_MESSAGES = 512;
export interface RunResult {
    status: 'completed' | 'failed' | 'exhausted' | 'cancelled';
    text: string;
    rounds: number;
    message?: string;
}

// The client owns history and the tool loop; the backend handles one round.
export class Conversation {
    serverUrl: string;
    workspace: string;
    model: string;
    maxRounds: number;
    ctx: HandlerContext;
    messages: Message[] = [];
    private running = false;

    constructor(serverUrl: string, workspace: string, model = '', maxRounds = 25) {
        const url = new URL(serverUrl);
        if (!['http:', 'https:'].includes(url.protocol)) throw new Error('server must use HTTP or HTTPS');
        if (!Number.isSafeInteger(maxRounds) || maxRounds < 1 || maxRounds > 100) throw new Error('maxRounds must be an integer between 1 and 100');
        this.serverUrl = serverUrl;
        this.ctx = new HandlerContext(workspace);
        this.workspace = this.ctx.workspace;
        this.model = model;
        this.maxRounds = maxRounds;
    }

    private append(messages: Message[]): void {
        const next = [...this.messages, ...messages];
        if (next.length > MAX_HISTORY_MESSAGES || Buffer.byteLength(JSON.stringify(next)) > MAX_HISTORY_BYTES) {
            throw new Error('conversation budget reached (1 MiB / 512 messages); start a new conversation');
        }
        this.messages.push(...messages);
    }

    async run(query: string, signal?: AbortSignal): Promise<RunResult> {
        if (this.running) return {status: 'failed', text: '', rounds: 0, message: 'conversation is already running'};
        this.running = true;
        this.ctx.signal = signal;
        let rounds = 0;
        try {
            signal?.throwIfAborted();
            if (typeof query !== 'string' || !query.trim()) throw new Error('query must be a non-empty string');
            this.append([{role: 'user', content: query}]);
            for (rounds = 1; rounds <= this.maxRounds; rounds++) {
                signal?.throwIfAborted();
                process.stdout.write(`\n--- round ${rounds} ---\n`);
                const result = await runRound(this.serverUrl, this.messages, this.workspace, this.model, signal);
                if (result.finish === FINISH.STOP) {
                    this.append([result.assistant]);
                    return {status: 'completed', text: result.assistant.content ?? '', rounds};
                }
                // Never execute a batch if no model round remains to inspect its results.
                if (rounds === this.maxRounds) return {status: 'exhausted', text: '', rounds, message: `reached maxRounds=${this.maxRounds}; last tool batch was not executed`};
                // Reserve space before any side effects. Tool results are bounded to 64 KiB.
                const reserved: Message[] = [result.assistant, ...result.toolCalls.map(call => ({role: 'tool', tool_call_id: call.id, content: ' '.repeat(64 * 1024)}))];
                const checkpoint = this.messages.length;
                this.append(reserved);
                this.messages.length = checkpoint;
                const replies: Message[] = [];
                for (const call of result.toolCalls) {
                    if (signal?.aborted) {
                        replies.push({role: 'tool', tool_call_id: call.id, content: 'ERROR: cancelled; tool was not executed'});
                    } else {
                        try { replies.push({role: 'tool', tool_call_id: call.id, content: await this.runTool(call)}); }
                        catch (e) {
                            if (!signal?.aborted) throw e;
                            replies.push({role: 'tool', tool_call_id: call.id, content: 'ERROR: cancelled; an executing command may have made partial changes'});
                        }
                    }
                }
                this.append([result.assistant, ...replies]);
                signal?.throwIfAborted();
            }
            throw new Error('unreachable round state');
        } catch (e) {
            return {status: signal?.aborted ? 'cancelled' : 'failed', text: '', rounds, message: (e as Error).message};
        } finally { this.ctx.signal = undefined; this.running = false; }
    }

    async runTool(call: ToolCall): Promise<string> {
        process.stdout.write(`→ ${call.name} ${JSON.stringify(call.arguments).slice(0, 120)}\n`);
        let result: string;
        try {
            const handler = createHandler(call.name, this.ctx);
            if (!handler) throw new ToolError(`unknown tool: ${call.name}`);
            result = await handler.execute(call.arguments);
        } catch (e) {
            if (this.ctx.signal?.aborted) throw e;
            result = `ERROR: ${(e as Error).message}`;
        }
        result = boundedOutput(result);
        process.stdout.write(result.slice(0, 240) + (result.length > 240 ? '...\n' : '\n'));
        return result;
    }
}
