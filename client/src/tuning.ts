export interface RetryPolicy {
    maxRetries: number;
    baseMs: number;
    capMs: number;
    jitter: number;
}

// Seven retries in addition to the initial request. History/compression is owned
// by the server; need_compression is fatal until its Phase 5 seam is implemented.
export const RETRY_POLICY: Readonly<RetryPolicy> = Object.freeze({maxRetries: 7, baseMs: 3000, capMs: 30000, jitter: 0.25});
export const STREAM_TIMEOUT_MS = 125_000;
export const NONSTREAM_TIMEOUT_MS = 125_000;

export function commandLimits(): {outputBytes: number; rssBytes: number; timeoutMs: number} {
    const positive = (key: string, fallback: number, max: number): number => {
        const value = process.env[key] === undefined ? fallback : Number(process.env[key]);
        if (!Number.isSafeInteger(value) || value < 1 || value > max) throw new Error(`${key} must be an integer between 1 and ${max}`);
        return value;
    };
    return {outputBytes: positive('RUN_OUTPUT_CAP', 1024 * 1024, 64 * 1024 * 1024), rssBytes: positive('RSS_CAP', 1536 * 1024 * 1024, Number.MAX_SAFE_INTEGER), timeoutMs: positive('RUN_TIMEOUT_MS', 120_000, 86_400_000)};
}
