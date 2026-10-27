import {readJSON} from './sessions.ts';

export async function printTrace(server: string, sid: string, round?: number): Promise<void> {
    if (!/^[A-Za-z0-9_-]{1,128}$/.test(sid) || (round !== undefined && (!Number.isSafeInteger(round) || round < 1))) throw new Error('invalid trace session or round');
    const body = await readJSON(server,`/trace/${sid}${round ? `?round=${round}` : ''}`,AbortSignal.timeout(10_000)) as {records: {round: number; traceId: string; stage: string; ts: number; code?: string; usage?: {total: number}; compact_latency_ms?: number; object_reads?: number; prompt_cache_prefix_stable?: boolean}[]};
    if (!body || !Array.isArray(body.records)) throw new Error('invalid trace response');
    let current = -1, total = 0;
    for (const record of body.records) {
        if (!record || !Number.isSafeInteger(record.round) || !Number.isSafeInteger(record.ts) || !/^[A-Za-z0-9_-]+$/.test(record.traceId) || !/^[a-z_]+$/.test(record.stage) || (record.code && !/^[a-z_]+$/.test(record.code)) || (record.usage && (!Number.isSafeInteger(record.usage.total) || record.usage.total < 0))) throw new Error('invalid trace record');
        if ((record.compact_latency_ms !== undefined && (!Number.isFinite(record.compact_latency_ms) || record.compact_latency_ms < 0)) || (record.object_reads !== undefined && (!Number.isSafeInteger(record.object_reads) || record.object_reads < 0)) || (record.prompt_cache_prefix_stable !== undefined && typeof record.prompt_cache_prefix_stable !== 'boolean')) throw new Error('invalid context metrics');
        if (record.round !== current) {current = record.round;process.stdout.write(`round ${current}\n`);}
        process.stdout.write(`${new Date(record.ts).toISOString()} ${record.traceId} ${record.stage}${record.code ? ` error{${record.code}}` : ''}${record.usage ? ` tokens=${record.usage.total}` : ''}\n`);
        if (record.object_reads !== undefined) process.stdout.write(`  compact_ms=${record.compact_latency_ms ?? 0} object_reads=${record.object_reads} prefix_stable=${record.prompt_cache_prefix_stable ?? 'unknown'}\n`);
        total += record.usage?.total ?? 0;
    }
    process.stdout.write(`observed tokens=${total} (best-effort trace; session ledger is authoritative)\n`);
}
