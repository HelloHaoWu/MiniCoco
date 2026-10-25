import type {Event} from './protocol.ts';

// Parse complete SSE frames, preserving UTF-8 boundaries and multi-line data.
export async function* parseSSE(stream: AsyncIterable<Uint8Array>): AsyncGenerator<Event> {
    let buffer = '', data: string[] = [], frameBytes = 0, totalBytes = 0;
    const decoder = new TextDecoder('utf-8', {fatal: true});
    for await (const chunk of stream) {
        totalBytes += chunk.byteLength;
        if (totalBytes > 4 * 1024 * 1024) throw new Error('response exceeds 4 MiB');
        buffer += decoder.decode(chunk, {stream: true});
        let newline: number;
        while ((newline = buffer.indexOf('\n')) >= 0) {
            const line = buffer.slice(0, newline).replace(/\r$/, '');
            buffer = buffer.slice(newline + 1);
            frameBytes += Buffer.byteLength(line) + 1;
            if (frameBytes > 1024 * 1024) throw new Error('SSE frame exceeds 1 MiB');
            if (line === '') {
                if (data.length) {
                    const event: unknown = JSON.parse(data.join('\n'));
                    if (!event || typeof event !== 'object' || Array.isArray(event) || typeof (event as Event).type !== 'string') {
                        throw new Error('invalid SSE event');
                    }
                    yield event as Event;
                }
                data = []; frameBytes = 0;
            } else if (line.startsWith('data:')) {
                data.push(line.slice(5).replace(/^ /, ''));
            }
        }
        if (Buffer.byteLength(buffer) + frameBytes > 1024 * 1024) throw new Error('SSE frame exceeds 1 MiB');
    }
    buffer += decoder.decode();
    if (buffer.length || data.length) throw new Error('incomplete SSE frame');
}
