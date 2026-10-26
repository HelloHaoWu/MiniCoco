import {runRound, runRoundNonStreaming, type RoundArgs, type RoundResult} from './executor.ts';
import {RoundError, abortError} from './roundError.ts';
import {RETRY_POLICY, type RetryPolicy} from './tuning.ts';

export interface RetryResult extends RoundResult { attempts: number; }
export interface RetryDependencies {
    run?: (args: RoundArgs, signal?: AbortSignal) => Promise<RoundResult>;
    runNonStreaming?: (args: RoundArgs, signal?: AbortSignal) => Promise<RoundResult>;
    sleep?: (attempt: number, policy: RetryPolicy, signal?: AbortSignal) => Promise<void>;
    onRetry?: (error: RoundError, nextAttempt: number) => void;
}

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

// Only this immutable delta is retried. Full history/compression now belong to
// the server. The first response's session ID is retained even if its body fails.
export async function runRoundWithRetry(args: RoundArgs, policy: RetryPolicy = RETRY_POLICY, signal?: AbortSignal, dependencies: RetryDependencies = {}): Promise<RetryResult> {
    let sessionId = args.sessionId, attempts = 0, retries = 0;
    const snapshot = {...args, backgroundResults: args.backgroundResults ? structuredClone(args.backgroundResults) : undefined, toolUseResults: args.toolUseResults ? structuredClone(args.toolUseResults) : undefined};
    const request = (): RoundArgs => ({...snapshot, sessionId, backgroundResults: snapshot.backgroundResults ? structuredClone(snapshot.backgroundResults) : undefined, toolUseResults: snapshot.toolUseResults ? structuredClone(snapshot.toolUseResults) : undefined,
        options: {...args.options, onSession: id => {sessionId = id;args.options?.onSession?.(id);}}});
    const run = dependencies.run ?? runRound;
    const fallback = dependencies.runNonStreaming ?? runRoundNonStreaming;
    if (!Number.isSafeInteger(policy.maxRetries) || policy.maxRetries < 0 || ![policy.baseMs, policy.capMs, policy.jitter].every(value => Number.isFinite(value) && value >= 0)) throw new RoundError('invalid retry policy', 'protocol');
    while (true) {
        try {
            if (signal?.aborted) throw abortError(signal);
            attempts++;
            const result = await run(request(), signal);
            if (signal?.aborted) throw abortError(signal);
            return {...result, attempts};
        } catch (cause) {
            const error = signal?.aborted ? abortError(signal) : cause instanceof RoundError ? cause : new RoundError((cause as Error).message, 'protocol', {cause});
            error.attempts = attempts;
            const classification = classify(error, !!signal?.aborted);
            if (classification === 'fatal') throw error;
            if (classification === 'compress') {
                const failure = new RoundError('cannot_compress: backend could not complete compression; no client history is available', 'backend', {code: 'cannot_compress', retryable: false, cause: error});
                failure.attempts = attempts;
                failure.traceId = error.traceId;
                throw failure;
            }
            try {
                if (retries >= policy.maxRetries) {
                    if (signal?.aborted) throw abortError(signal);
                    attempts++;
                    process.stderr.write(`\n[fallback] stream retries exhausted; one non-streaming attempt ${attempts}\n`);
                    const result = await fallback(request(), signal);
                    if (signal?.aborted) throw abortError(signal);
                    return {...result, attempts};
                }
                (dependencies.onRetry ?? ((failure, nextAttempt) => process.stderr.write(`\n[retry] ${failure.code ?? failure.kind}; discarded incomplete round, next attempt ${nextAttempt}\n`)))(error, attempts + 1);
                await (dependencies.sleep ?? backoffSleep)(retries++, policy, signal);
            } catch (failure) {
                const final = signal?.aborted ? abortError(signal) : failure instanceof RoundError ? failure : new RoundError((failure as Error).message, 'protocol', {cause: failure});
                final.attempts = attempts;
                throw final;
            }
        }
    }
}
