import {runRound, runRoundNonStreaming, type RoundArgs, type RoundResult} from './executor.ts';
import {RoundError, abortError} from './roundError.ts';
import type {Message} from './protocol.ts';
import {RETRY_POLICY, type RetryPolicy} from './tuning.ts';

export interface CompressionHook { compress(messages: Message[], reason: 'upstream' | 'proactive', signal?: AbortSignal): Promise<Message[]>; }
export interface RetryResult extends RoundResult { messages: Message[]; attempts: number; }
export interface RetryDependencies {
    run?: (args: RoundArgs, signal?: AbortSignal) => Promise<RoundResult>;
    runNonStreaming?: (args: RoundArgs, signal?: AbortSignal) => Promise<RoundResult>;
    sleep?: (attempt: number, policy: RetryPolicy, signal?: AbortSignal) => Promise<void>;
    onRetry?: (error: RoundError, nextAttempt: number) => void;
}
export const NOOP_COMPRESSION: CompressionHook = {async compress() {throw new RoundError('cannot compress: no compression implementation configured', 'protocol', {code: 'cannot_compress'});}};

export function classify(error: RoundError, userAborted = false): 'transient' | 'fatal' | 'compress' {
    if (userAborted) return 'fatal';
    if (error.kind === 'backend' && error.code === 'need_compression') return 'compress';
    if (error.kind === 'abort') return 'fatal';
    if (error.kind === 'network' || error.kind === 'timeout') return 'transient';
    if (error.kind === 'backend') return error.retryable === true ? 'transient' : 'fatal';
    if (error.kind === 'http') {
        const status = error.httpStatus ?? 0;
        return (status >= 500 && status <= 599) || [408, 425, 429].includes(status) ? 'transient' : 'fatal';
    }
    return 'fatal';
}

export function backoffDelay(attempt: number, policy: RetryPolicy, random = Math.random): number {
    return Math.min(policy.capMs, policy.baseMs * 2 ** attempt) * (1 + random() * policy.jitter);
}

export function backoffSleep(attempt: number, policy: RetryPolicy, signal?: AbortSignal): Promise<void> {
    if (signal?.aborted) return Promise.reject(abortError(signal));
    return new Promise((resolve, reject) => {
        const finish = (): void => {signal?.removeEventListener('abort', abort);resolve();};
        const timer = setTimeout(finish, backoffDelay(attempt, policy));
        const abort = (): void => {clearTimeout(timer);signal?.removeEventListener('abort', abort);reject(abortError(signal));};
        signal?.addEventListener('abort', abort, {once: true});
        if (signal?.aborted) abort();
    });
}

async function cancellable<T>(task: Promise<T>, signal?: AbortSignal): Promise<T> {
    if (!signal) return task;
    return new Promise((resolve, reject) => {
        const abort = (): void => {signal.removeEventListener('abort', abort);reject(abortError(signal));};
        signal.addEventListener('abort', abort, {once: true});
        task.then(value => {signal.removeEventListener('abort', abort);resolve(value);}, error => {signal.removeEventListener('abort', abort);reject(error);});
        if (signal.aborted) abort();
    });
}

function validCompression(previous: Message[], next: Message[]): boolean {
    if (!Array.isArray(next) || next.length === 0 || Buffer.byteLength(JSON.stringify(next)) >= Buffer.byteLength(JSON.stringify(previous)) || JSON.stringify(next.at(-1)) !== JSON.stringify(previous.at(-1))) return false;
    const pending = new Set<string>();
    for (const message of next) {
        if (!message || !['user', 'assistant', 'tool'].includes(message.role) || (message.content !== undefined && message.content !== null && typeof message.content !== 'string')) return false;
        if (message.role === 'tool') {
            if (!message.tool_call_id || !pending.delete(message.tool_call_id)) return false;
        } else {
            if (pending.size) return false;
            for (const call of message.tool_calls ?? []) {
                if (message.role !== 'assistant' || !call.id || pending.has(call.id) || call.type !== 'function' || !call.function?.name || typeof call.function.arguments !== 'string') return false;
                pending.add(call.id);
            }
        }
    }
    return pending.size === 0;
}

export async function runRoundWithRetry(args: RoundArgs, policy: RetryPolicy = RETRY_POLICY, signal?: AbortSignal, hook: CompressionHook = NOOP_COMPRESSION, dependencies: RetryDependencies = {}): Promise<RetryResult> {
    let messages = structuredClone(args.messages), attempts = 0, retries = 0, compressions = 0;
    const run = dependencies.run ?? ((input, currentSignal) => runRound(input.serverUrl, input.messages, input.workspace, input.model, currentSignal, input.options));
    const fallback = dependencies.runNonStreaming ?? ((input: RoundArgs, currentSignal?: AbortSignal) => runRoundNonStreaming(input.serverUrl, input.messages, input.workspace, input.model, currentSignal, input.options));
    const sleep = dependencies.sleep ?? backoffSleep;
    if (!Number.isSafeInteger(policy.maxRetries) || policy.maxRetries < 0 || !Number.isSafeInteger(policy.maxCompressions) || policy.maxCompressions < 0 || ![policy.baseMs, policy.capMs, policy.jitter].every(value => Number.isFinite(value) && value >= 0)) throw new RoundError('invalid retry policy', 'protocol');
    while (true) {
        try {
            if (signal?.aborted) throw abortError(signal);
            attempts++;
            const result = await run({...args, messages: structuredClone(messages)}, signal);
            if (signal?.aborted) throw abortError(signal);
            return {...result, messages, attempts};
        } catch (cause) {
            const error = signal?.aborted ? abortError(signal) : cause instanceof RoundError ? cause : new RoundError((cause as Error).message, 'protocol', {cause});
            error.attempts = attempts;
            const classification = classify(error, !!signal?.aborted);
            if (classification === 'fatal') throw error;
            try {
                if (classification === 'compress') {
                    if (compressions++ >= policy.maxCompressions) throw error;
                    const next = await cancellable(hook.compress(structuredClone(messages), 'upstream', signal), signal);
                    if (!validCompression(messages, next)) throw new RoundError('compression must reduce serialized history and preserve the current input and complete tool batches', 'protocol', {code: 'cannot_compress'});
                    messages = structuredClone(next);
                    continue;
                }
                if (retries >= policy.maxRetries) {
                    if (signal?.aborted) throw abortError(signal);
                    attempts++;
                    process.stderr.write(`\n[fallback] stream retries exhausted; one non-streaming attempt ${attempts}\n`);
                    const result = await fallback({...args, messages: structuredClone(messages)}, signal);
                    if (signal?.aborted) throw abortError(signal);
                    return {...result, messages, attempts};
                }
                (dependencies.onRetry ?? ((failure, nextAttempt) => process.stderr.write(`\n[retry] ${failure.code ?? failure.kind}; discarded incomplete round, next attempt ${nextAttempt}\n`)))(error, attempts + 1);
                await sleep(retries++, policy, signal);
            } catch (failure) {
                const final = signal?.aborted ? abortError(signal) : failure instanceof RoundError ? failure : new RoundError((failure as Error).message, 'protocol', {cause: failure});
                final.attempts = attempts;
                throw final;
            }
        }
    }
}
