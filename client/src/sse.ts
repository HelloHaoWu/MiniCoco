import type {Event} from './protocol.ts';

// SSE stream parser.
//
// Mirrors Coco's shared-internals/SSEProcessor: read a streaming HTTP
// response chunk by chunk, split on newlines, strip the `data:` prefix, and
// JSON-parse each event.
export async function* parseSSE(stream: AsyncIterable<Buffer>): AsyncGenerator<Event> {
    let buffer = '';
    const decoder = new TextDecoder();
    for await (const chunk of stream) {
        buffer += decoder.decode(chunk, {stream: true});
        const lines = buffer.split('\n');
        buffer = lines.pop() ?? ''; // keep the last, possibly-partial line
        for (const line of lines) {
            if (!line.startsWith('data:')) {
                continue; // ignore heartbeats / blank separators
            }
            const payload = line.slice('data:'.length).trim();
            if (!payload) {
                continue;
            }
            try {
                yield JSON.parse(payload) as Event;
            } catch {
                // ignore a malformed line rather than killing the stream
            }
        }
    }
}

