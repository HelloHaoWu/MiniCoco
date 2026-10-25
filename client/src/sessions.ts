import type {Session} from './protocol.ts';

async function readJSON(server: string, suffix: string, signal?: AbortSignal): Promise<unknown> {
    const url = new URL(server);
    if (!['http:', 'https:'].includes(url.protocol)) throw new Error('server must use HTTP or HTTPS');
    const timeout = AbortSignal.timeout(125_000);
    const response = await fetch(server.replace(/\/$/, '') + suffix, {signal: signal ? AbortSignal.any([signal, timeout]) : timeout});
    try {
        if (!response.ok) throw new Error(`session request failed: HTTP ${response.status}`);
        if (!response.headers.get('content-type')?.startsWith('application/json') || !response.body) throw new Error('invalid session response');
        const reader = response.body.getReader();
        const buffers: Uint8Array[] = [];let bytes = 0;
        try {
            while (true) {
                const next = await reader.read();
                if (next.done) break;
                bytes += next.value.byteLength;
                if (bytes > 4 * 1024 * 1024) throw new Error('session response exceeds 4 MiB');
                buffers.push(next.value);
            }
        } finally {reader.releaseLock();}
        return JSON.parse(new TextDecoder('utf-8', {fatal: true}).decode(Buffer.concat(buffers)));
    } finally {if (response.body && !response.body.locked) await response.body.cancel().catch(() => {});}
}

function isSession(value: unknown): value is Session {
    if (!value || typeof value !== 'object') return false;
    const session = value as Session;
    return typeof session.id === 'string' && /^[A-Za-z0-9_-]{1,128}$/.test(session.id) && typeof session.workspace === 'string' && typeof session.model === 'string' &&
        typeof session.user === 'string' && typeof session.ended === 'boolean' &&
        [session.created_at, session.message_count, session.last_message_count, session.prompt_tokens, session.completion_tokens, session.total_tokens].every(n => Number.isInteger(n) && n >= 0);
}

export async function listSessions(server: string, signal?: AbortSignal): Promise<Session[]> {
    const data = await readJSON(server, '/sessions', signal) as {sessions?: unknown};
    if (!data || !Array.isArray(data.sessions) || !data.sessions.every(isSession)) throw new Error('invalid session list');
    return data.sessions;
}

export async function getSession(server: string, id: string, signal?: AbortSignal): Promise<{session: Session; canResume: boolean}> {
    if (!/^[A-Za-z0-9_-]{1,128}$/.test(id)) throw new Error('invalid session ID');
    const data = await readJSON(server, '/sessions/' + encodeURIComponent(id), signal) as {session: Session; canResume: boolean};
    if (!data || !isSession(data.session) || data.session.id !== id || typeof data.canResume !== 'boolean') throw new Error('invalid session metadata');
    return data;
}
