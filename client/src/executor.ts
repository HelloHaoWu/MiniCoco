import {parseSSE} from './sse.ts';
import {EV, FINISH, type Event, type Message, type ToolCall} from './protocol.ts';
import {RoundError, abortError} from './roundError.ts';
import {STREAM_TIMEOUT_MS} from './tuning.ts';
export {RoundError} from './roundError.ts';

export interface RoundResult { assistant: Message; toolCalls: ToolCall[]; finish: string; }
export interface RoundOptions { timeoutMs?: number; onEvent?: (event: Event) => void; }
export interface RoundArgs { serverUrl: string; messages: Message[]; workspace: string; model: string; options?: RoundOptions; }

// No I/O or conversation mutation: one validator for all event transports.
export async function consumeEvents(source: Iterable<Event> | AsyncIterable<Event>): Promise<RoundResult> {
    const text: string[] = [], toolCalls: ToolCall[] = [];
    const ids = new Set<string>();
    let finish = '';
    for await (const event of source) {
        if (!event || typeof event !== 'object' || Array.isArray(event) || typeof event.type !== 'string' || finish) throw new RoundError('invalid event or data after done', 'protocol');
        if (event.type === EV.TEXT || event.type === EV.REASONING) {
            if (typeof event.delta !== 'string') throw new RoundError('invalid text delta', 'protocol');
            if (event.type === EV.TEXT) text.push(event.delta);
        } else if (event.type === EV.TOOL_CALL) {
            if (typeof event.id !== 'string' || !event.id || event.id.length > 256 || typeof event.name !== 'string' || !event.name || event.name.length > 128 ||
                !event.arguments || typeof event.arguments !== 'object' || Array.isArray(event.arguments) || ids.has(event.id)) throw new RoundError('invalid or duplicate tool call', 'protocol');
            if (toolCalls.length >= 16) throw new RoundError('too many tool calls in one round', 'protocol');
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

export async function runRound(serverUrl: string, messages: Message[], workspace: string, model: string, signal?: AbortSignal, options: RoundOptions = {}): Promise<RoundResult> {
    if (signal?.aborted) throw abortError(signal);
    const timeout = AbortSignal.timeout(options.timeoutMs ?? STREAM_TIMEOUT_MS);
    let response: Response | undefined;
    let fetching = true, printedReasoning = false;
    const render = options.onEvent ?? ((event: Event): void => {
        if (event.type === EV.REASONING && !printedReasoning) {process.stdout.write('\n\x1b[2m[thinking] ');printedReasoning = true;}
        if (event.type === EV.TEXT || event.type === EV.REASONING) process.stdout.write(event.delta!);
    });
    try {
        try { const url = new URL(serverUrl); if (!['http:', 'https:'].includes(url.protocol)) throw new Error('unsupported protocol'); }
        catch (cause) { throw new RoundError('server must be a valid HTTP(S) URL', 'protocol', {cause}); }
        response = await fetch(serverUrl.replace(/\/$/, '') + '/chat/stream', {
            method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({messages, workspace, model}),
            signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
        });
        fetching = false;
        if (!response.ok) throw new RoundError(`backend HTTP ${response.status}`, 'http', {httpStatus: response.status});
        if (!response.headers.get('content-type')?.toLowerCase().startsWith('text/event-stream') || !response.body) throw new RoundError('backend did not return an SSE stream', 'protocol');
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
        return await consumeEvents(events());
    } catch (cause) {
        if (signal?.aborted) throw abortError(signal);
        if (timeout.aborted) throw new RoundError('model request timed out', 'timeout', {cause});
        if (cause instanceof RoundError) throw cause;
        throw new RoundError((cause as Error).message, fetching ? 'network' : 'protocol', {cause});
    } finally {
        if (printedReasoning) process.stdout.write('\x1b[0m\n');
        if (response?.body && !response.body.locked) await response.body.cancel().catch(() => {});
    }
}
