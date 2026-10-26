import {parseSSE} from './sse.ts';
import {EV, FINISH, type Event, type Message, type ToolCall, type RoundRequest, type ToolMode, type MCPToolSpec} from './protocol.ts';
import {normalizeMode, toolAllowed} from './toolMode.ts';
import {RoundError, abortError} from './roundError.ts';
import {STREAM_TIMEOUT_MS, NONSTREAM_TIMEOUT_MS} from './tuning.ts';
export {RoundError} from './roundError.ts';

export interface RoundResult { assistant: Message; toolCalls: ToolCall[]; finish: string; sessionId?: string; traceId?: string; }
export interface RoundOptions { timeoutMs?: number; onEvent?: (event: Event) => void; onSession?: (id: string) => void; }
export interface RoundArgs extends RoundRequest { serverUrl: string; options?: RoundOptions; }

// No I/O or conversation mutation: one validator for all event transports.
export async function consumeEvents(source: Iterable<Event> | AsyncIterable<Event>, mode: ToolMode = 'full', catalog: MCPToolSpec[] = []): Promise<RoundResult> {
    mode = normalizeMode(mode);
    const text: string[] = [], toolCalls: ToolCall[] = [];
    const ids = new Set<string>();
    let finish = '';
    for await (const event of source) {
        if (!event || typeof event !== 'object' || Array.isArray(event) || typeof event.type !== 'string' || finish) throw new RoundError('invalid event or data after done', 'protocol');
        if (Buffer.byteLength(JSON.stringify(event)) > 1024 * 1024) throw new RoundError('event exceeds 1 MiB', 'protocol');
        if (event.type === EV.TEXT || event.type === EV.REASONING) {
            if (typeof event.delta !== 'string') throw new RoundError('invalid text delta', 'protocol');
            if (event.type === EV.TEXT) text.push(event.delta);
        } else if (event.type === EV.TOOL_CALL) {
            if (typeof event.id !== 'string' || !event.id || event.id.length > 256 || typeof event.name !== 'string' || !event.name || event.name.length > 128 ||
                !event.arguments || typeof event.arguments !== 'object' || Array.isArray(event.arguments) || ids.has(event.id)) throw new RoundError('invalid or duplicate tool call', 'protocol');
            if (toolCalls.length >= 16) throw new RoundError('too many tool calls in one round', 'protocol');
            if (!toolAllowed(mode, event.name, catalog)) throw new RoundError(`tool ${event.name} is not allowed in mode ${mode}`, 'protocol');
            ids.add(event.id);
            toolCalls.push({id: event.id, name: event.name, arguments: event.arguments});
        } else if (event.type === EV.ERROR) {
            if (typeof event.message !== 'string' || (event.code !== undefined && typeof event.code !== 'string') || (event.retryable !== undefined && typeof event.retryable !== 'boolean')) throw new RoundError('invalid error event', 'protocol');
            throw new RoundError(`backend error: ${event.message}`, 'backend', {code: event.code, retryable: event.retryable === true});
        } else if (event.type === EV.DONE) {
            if (event.finish_reason !== FINISH.STOP && event.finish_reason !== FINISH.TOOL_CALLS) throw new RoundError(`unsuccessful finish: ${event.finish_reason}`, 'protocol');
            finish = event.finish_reason;
        } else throw new RoundError(`unknown event type: ${event.type}`, 'protocol');
    }
    if (!finish) throw new RoundError('incomplete response: missing done', 'network');
    if ((finish === FINISH.TOOL_CALLS) !== (toolCalls.length > 0)) throw new RoundError('finish reason does not match tool calls', 'protocol');
    const assistant: Message = {role: 'assistant', content: text.join('') || null};
    if (toolCalls.length) assistant.tool_calls = toolCalls.map(call => ({id: call.id, type: 'function', function: {name: call.name, arguments: JSON.stringify(call.arguments)}}));
    return {assistant, toolCalls, finish};
}

export async function runRound(args: RoundArgs, signal?: AbortSignal): Promise<RoundResult> {
    return executeRound(args, true, signal);
}

export async function runRoundNonStreaming(args: RoundArgs, signal?: AbortSignal): Promise<RoundResult> {
    return executeRound(args, false, signal);
}

async function executeRound(args: RoundArgs, stream: boolean, signal?: AbortSignal): Promise<RoundResult> {
    const {serverUrl, options = {}, ...request} = args;
    if (signal?.aborted) throw abortError(signal);
    const timeout = AbortSignal.timeout(options.timeoutMs ?? (stream ? STREAM_TIMEOUT_MS : NONSTREAM_TIMEOUT_MS));
    let response: Response | undefined;
    let traceId: string | undefined;
    let fetching = true, printedReasoning = false;
    const render = options.onEvent ?? ((event: Event): void => {
        if (event.type === EV.REASONING && !printedReasoning) {process.stdout.write('\n\x1b[2m[thinking] ');printedReasoning = true;}
        if (event.type === EV.TEXT || event.type === EV.REASONING) process.stdout.write(event.delta!);
    });
    try {
        const mode = normalizeMode(args.mode);
        try { const url = new URL(serverUrl); if (!['http:', 'https:'].includes(url.protocol)) throw new Error('unsupported protocol'); }
        catch (cause) { throw new RoundError('server must be a valid HTTP(S) URL', 'protocol', {cause}); }
        response = await fetch(serverUrl.replace(/\/$/, '') + '/chat/stream', {
            method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({...request, stream}),
            signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
        });
        fetching = false;
        const trace = response.headers.get('x-trace-id');
        if (trace && /^[A-Za-z0-9_-]{1,160}$/.test(trace)) traceId = trace;
        const sessionId = response.headers.get('x-session-id') ?? '';
        if (sessionId) {
            if (!/^[A-Za-z0-9_-]{1,128}$/.test(sessionId) || (args.sessionId && sessionId !== args.sessionId)) throw new RoundError('invalid or changed session ID', 'protocol');
            options.onSession?.(sessionId);
        }
        if (!response.ok) throw new RoundError(`backend HTTP ${response.status}`, 'http', {httpStatus: response.status});
        if (!sessionId) throw new RoundError('missing X-Session-Id response header', 'protocol');
        const expectedType = stream ? 'text/event-stream' : 'application/json';
        if (!response.headers.get('content-type')?.toLowerCase().startsWith(expectedType) || !response.body) throw new RoundError(`backend did not return ${expectedType}`, 'protocol');
        const reader = response.body.getReader();
        async function* chunks(): AsyncGenerator<Uint8Array> {
            try {
                while (true) {
                    let next: ReadableStreamReadResult<Uint8Array>;
                    try { next = await reader.read(); }
                    catch (cause) { throw new RoundError('response stream interrupted', 'network', {cause}); }
                    if (next.done) return;
                    yield next.value;
                }
            } finally { reader.releaseLock(); }
        }
        async function* events(): AsyncGenerator<Event> {
            for await (const event of parseSSE(chunks())) {
                yield event; // The consumer validates before rendering is resumed.
                render(event);
                if (event.type === EV.DONE) return;
            }
        }
        if (!stream) {
            const buffers: Uint8Array[] = [];let bytes = 0;
            for await (const chunk of chunks()) {
                bytes += chunk.byteLength;
                if (bytes > 4 * 1024 * 1024) throw new RoundError('JSON response exceeds 4 MiB', 'protocol');
                buffers.push(chunk);
            }
            let envelope: unknown;
            try { envelope = JSON.parse(new TextDecoder('utf-8', {fatal: true}).decode(Buffer.concat(buffers))); }
            catch (cause) { throw new RoundError('invalid JSON response', 'protocol', {cause}); }
            if (!envelope || typeof envelope !== 'object' || !('events' in envelope) || !Array.isArray(envelope.events)) throw new RoundError('JSON response must contain an events array', 'protocol');
            const result = await consumeEvents(envelope.events as Event[], mode, args.mcpTools);
            for (const event of envelope.events as Event[]) render(event);
            return {...result, sessionId, traceId};
        }
        return {...await consumeEvents(events(), mode, args.mcpTools), sessionId, traceId};
    } catch (cause) {
        const error = signal?.aborted ? abortError(signal) : timeout.aborted ? new RoundError('model request timed out', 'timeout', {cause}) : cause instanceof RoundError ? cause : new RoundError((cause as Error).message, fetching ? 'network' : 'protocol', {cause});
        error.traceId = traceId;
        throw error;
    } finally {
        if (printedReasoning) process.stdout.write('\x1b[0m\n');
        if (response?.body && !response.body.locked) await response.body.cancel().catch(() => {});
    }
}
