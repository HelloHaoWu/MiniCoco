export interface AuditRecord {ts: number; sessionId: string; traceId: string; tool: string; argsDigest: string; approved: boolean; status: 'ok' | 'error' | 'denied'; ms: number;}

// Transport to the backend's shared JSONL sink. One in-flight request and a
// bounded discard-oldest queue keep audit failures out of tool execution.
export class AuditClient {
    private queue: AuditRecord[] = [];
    private writing?: Promise<void>;
    private server: string;
    constructor(server: string) {this.server = server.replace(/\/$/, '');}
    emit(record: AuditRecord): void {
        if (!record.sessionId) return;
        if (this.queue.length === 1024) this.queue.shift();
        this.queue.push(record);
        this.writing ??= this.drain();
    }
    private async drain(): Promise<void> {
        try {
            while (this.queue.length) {
                const record = this.queue.shift()!;
                try {const response = await fetch(this.server + '/audit', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(record), signal: AbortSignal.timeout(500)});await response.body?.cancel();} catch { /* Best effort. */ }
            }
        } finally {this.writing = undefined;}
    }
    async flush(): Promise<void> {
        let timer: ReturnType<typeof setTimeout> | undefined;
        try {await Promise.race([this.writing, new Promise<void>(resolve => {timer = setTimeout(resolve, 1500);})]);}
        finally {if (timer) clearTimeout(timer);this.queue = [];}
    }
}
