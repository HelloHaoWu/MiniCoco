import {parseSSE} from './sse.ts';
import {EV, FINISH, type Message, type ToolCall} from './protocol.ts';

export interface RoundResult { assistant: Message; toolCalls: ToolCall[]; finish: string; }

export async function runRound(serverUrl: string, messages: Message[], workspace: string, model: string, signal?: AbortSignal): Promise<RoundResult> {
    const timeout = AbortSignal.timeout(125_000);
    const response = await fetch(serverUrl.replace(/\/$/, '') + '/chat/stream', {
        method: 'POST', headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({messages, workspace, model}),
        signal: signal ? AbortSignal.any([signal, timeout]) : timeout,
    });
    const text: string[] = [], toolCalls: ToolCall[] = [];
    const ids = new Set<string>();
    let finish = '', printedReasoning = false;
    try {
        if (!response.ok) throw new Error(`backend HTTP ${response.status}`);
        if (!response.headers.get('content-type')?.toLowerCase().startsWith('text/event-stream') || !response.body) {
            throw new Error('backend did not return an SSE stream');
        }
        const reader = response.body.getReader();
        async function* chunks(): AsyncGenerator<Uint8Array> {
            try {
                while (true) {
                    const {value, done} = await reader.read();
                    if (done) return;
                    yield value;
                }
            } finally { reader.releaseLock(); }
        }
        for await (const event of parseSSE(chunks())) {
            if (event.type === EV.TEXT || event.type === EV.REASONING) {
                if (typeof event.delta !== 'string') throw new Error('invalid text delta');
                if (event.type === EV.TEXT) text.push(event.delta);
                else if (!printedReasoning) { process.stdout.write('\n\x1b[2m[thinking] '); printedReasoning = true; }
                process.stdout.write(event.delta);
            } else if (event.type === EV.TOOL_CALL) {
                if (!event.id || !event.name || typeof event.id !== 'string' || typeof event.name !== 'string' ||
                    !event.arguments || typeof event.arguments !== 'object' || Array.isArray(event.arguments) || ids.has(event.id)) {
                    throw new Error('invalid or duplicate tool call');
                }
                if (toolCalls.length >= 16) throw new Error('too many tool calls in one round');
                ids.add(event.id);
                toolCalls.push({id: event.id, name: event.name, arguments: event.arguments});
            } else if (event.type === EV.ERROR) {
                throw new Error(`backend error: ${event.message ?? 'unknown error'}`);
            } else if (event.type === EV.DONE) {
                finish = event.finish_reason ?? '';
                break;
            } else throw new Error(`unknown event type: ${event.type}`);
        }
        if (finish !== FINISH.STOP && finish !== FINISH.TOOL_CALLS) throw new Error(`incomplete or unsuccessful response: ${finish || 'missing done'}`);
        if ((finish === FINISH.TOOL_CALLS) !== (toolCalls.length > 0)) throw new Error('finish reason does not match tool calls');
        const assistant: Message = {role: 'assistant', content: text.join('') || null};
        if (toolCalls.length) assistant.tool_calls = toolCalls.map(call => ({
            id: call.id, type: 'function', function: {name: call.name, arguments: JSON.stringify(call.arguments)},
        }));
        return {assistant, toolCalls, finish};
    } finally {
        if (printedReasoning) process.stdout.write('\x1b[0m\n');
        if (response.body && !response.body.locked) await response.body.cancel().catch(() => {});
    }
}
