import type {Session} from './protocol.ts';

export async function readJSON(server: string, suffix: string, signal?: AbortSignal, body?: unknown): Promise<unknown> {
    const url = new URL(server);
    if (!['http:', 'https:'].includes(url.protocol)) throw new Error('server must use HTTP or HTTPS');
    const timeout = AbortSignal.timeout(125_000);
    const response = await fetch(server.replace(/\/$/, '') + suffix, {method: body === undefined ? 'GET' : 'POST', headers: body === undefined ? undefined : {'Content-Type':'application/json'}, body: body === undefined ? undefined : JSON.stringify(body), signal: signal ? AbortSignal.any([signal, timeout]) : timeout});
    try {
        if (!response.ok) {
            let code = '';
            if (response.body) {
                const reader = response.body.getReader();const chunks: Uint8Array[] = [];let bytes = 0;
                try {while (bytes <= 4096) {const next = await reader.read();if(next.done)break;bytes+=next.value.byteLength;if(bytes<=4096)chunks.push(next.value);}
                    if(bytes<=4096){const payload=JSON.parse(Buffer.concat(chunks).toString('utf8')) as {code?:unknown};if(typeof payload.code==='string'&&/^[A-Za-z_]{1,64}$/.test(payload.code))code=payload.code;}
                } catch { /* Keep the HTTP status for non-JSON failures. */ } finally {await reader.cancel().catch(()=>{});reader.releaseLock();}
            }
            throw new Error(`session request failed: HTTP ${response.status}${code ? ` (${code})` : ''}`);
        }
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
    if (session.agentType !== undefined && session.agentType !== 'explore') return false;
    if (session.parentSessionId !== undefined && session.parentSessionId !== null && (typeof session.parentSessionId !== 'string' || !/^[A-Za-z0-9_-]{1,128}$/.test(session.parentSessionId))) return false;
    if (session.agentType && !session.parentSessionId) return false;
    if (session.forkedFrom !== undefined && (typeof session.forkedFrom !== 'string' || !/^[A-Za-z0-9_-]{1,128}$/.test(session.forkedFrom))) return false;
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

export async function forkSession(server:string,sessionId:string,forkMessageId?:string):Promise<{newSessionId:string;resolvedForkMessageId:string;copiedThroughMessageId:string}>{
    if(!/^[A-Za-z0-9_-]{1,128}$/.test(sessionId)||(forkMessageId!==undefined&&!/^(message|assistant):[1-9][0-9]*$/.test(forkMessageId)))throw new Error('fork requires a session and optional message:N or assistant:N');
    const result=await readJSON(server,'/fork',undefined,{sessionId,forkMessageId}) as {newSessionId:string;resolvedForkMessageId:string;copiedThroughMessageId:string};
    if(!result||!/^[A-Za-z0-9_-]{1,128}$/.test(result.newSessionId)||!/^message:[1-9][0-9]*$/.test(result.resolvedForkMessageId)||!/^message:[1-9][0-9]*$/.test(result.copiedThroughMessageId))throw new Error('invalid fork response');
    return result;
}
