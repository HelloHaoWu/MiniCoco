import * as http from 'node:http';
import {parseSSE} from './sse.ts';
import {EV, FINISH, type Message, type ToolCall} from './protocol.ts';

// One round of the loop: call the backend, consume its SSE stream.
//
// Mirrors Coco's AgentExecutor.executeStream / processStream: fire one
// streaming request, render text deltas as they arrive, and collect the
// assistant message + any tool calls the model asked for.

export interface RoundResult {
    assistant: Message;
    toolCalls: ToolCall[];
    finish: string; 
}

function postStream(serverUrl: string, body: unknown): Promise<http.IncomingMessage> {
    const url = new URL(serverUrl + '/chat/stream');
    const data = Buffer.from(JSON.stringify(body));
    return new Promise((resolve, reject) => {
        const req = http.request(
            url,
            {method: 'POST', headers: {'Content-Type': 'application/json', 'Content-Length': String(data.length)}},
            resolve
        );
        req.on('error', reject);
        req.write(data);
        req.end();
    });
}

export async function runRound(
    serverUrl: string,
    messages: Message[],
    workspace: string,
    model: string
): Promise<RoundResult> {
    const res = await postStream(serverUrl, {messages, workspace, model});
    const textParts: string[] = [];
    const toolCalls: ToolCall[] = [];
    let finish: string = FINISH.STOP;
    let printedReasoning = false;

    for await (const ev of parseSSE(res as AsyncIterable<Buffer>)) {
        if (ev.type === EV.TEXT) {
            process.stdout.write(ev.delta ?? '');
            textParts.push(ev.delta ?? '');
        } else if (ev.type === EV.REASONING) {
            if (!printedReasoning) {
                process.stdout.write('\n\x1b[2m[thinking] ');
                printedReasoning = true;
            }
            process.stdout.write(ev.delta ?? '');
        } else if (ev.type === EV.TOOL_CALL) {
            toolCalls.push({id: ev.id ?? '', name: ev.name ?? '', arguments: ev.arguments ?? {}});
        } else if (ev.type === EV.ERROR) {
            process.stdout.write(`\n[backend error] ${ev.message}\n`);
        } else if (ev.type === EV.DONE) {
            finish = ev.finish_reason ?? FINISH.STOP;
        }
    }
    if (printedReasoning) {
        process.stdout.write('\x1b[0m\n');
    }

    const assistant: Message = {role: 'assistant', content: textParts.join('') || null};
    if (toolCalls.length) {
        assistant.tool_calls = toolCalls.map(tc => ({
            id: tc.id,
            type: 'function' as const,
            function: {name: tc.name, arguments: JSON.stringify(tc.arguments)},
        }));
    }
    return {assistant, toolCalls, finish};
}

