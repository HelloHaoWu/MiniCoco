import type {Event} from './protocol.ts';
import {RoundError} from './roundError.ts';

// Parse complete SSE frames, preserving UTF-8 boundaries and multi-line data.
export async function* parseSSE(stream: AsyncIterable<Uint8Array>): AsyncGenerator<Event> {
    let buffer = '', data: string[] = [], frameBytes = 0, totalBytes = 0;
    const decoder = new TextDecoder('utf-8', {fatal: true});
    for await (const chunk of stream) {
        totalBytes += chunk.byteLength;
        if (totalBytes > 4 * 1024 * 1024) throw new RoundError('response exceeds 4 MiB', 'protocol');
        try { buffer += decoder.decode(chunk, {stream: true}); }
        catch (cause) { throw new RoundError('invalid SSE UTF-8', 'protocol', {cause}); }
        let newline: number;
        while ((newline = buffer.indexOf('\n')) >= 0) {
            const line = buffer.slice(0, newline).replace(/\r$/, '');
            buffer = buffer.slice(newline + 1);
            frameBytes += Buffer.byteLength(line) + 1;
            if (frameBytes > 1024 * 1024) throw new RoundError('SSE frame exceeds 1 MiB', 'protocol');
            if (line === '') {
                if (data.length) {
                    let event: unknown;
                    try { event = JSON.parse(data.join('\n')); }
                    catch (cause) { throw new RoundError('invalid SSE JSON', 'protocol', {cause}); }
                    if (!event || typeof event !== 'object' || Array.isArray(event) || typeof (event as Event).type !== 'string') {
                        throw new RoundError('invalid SSE event', 'protocol');
                    }
                    yield event as Event;
                }
                data = []; frameBytes = 0;
            } else if (line.startsWith('data:')) {
                data.push(line.slice(5).replace(/^ /, ''));
            }
        }
        if (Buffer.byteLength(buffer) + frameBytes > 1024 * 1024) throw new RoundError('SSE frame exceeds 1 MiB', 'protocol');
    }
    try { buffer += decoder.decode(); }
    catch (cause) { throw new RoundError('incomplete SSE UTF-8', 'network', {cause}); }
    if (buffer.length || data.length) throw new RoundError('incomplete SSE frame', 'network');
}
