export interface RetryPolicy {
    maxRetries: number;
    baseMs: number;
    capMs: number;
    jitter: number;
    maxCompressions: number;
}

// Seven retries in addition to the initial request, independently of compression.
export const RETRY_POLICY: Readonly<RetryPolicy> = Object.freeze({maxRetries: 7, baseMs: 3000, capMs: 30000, jitter: 0.25, maxCompressions: 3});
export const STREAM_TIMEOUT_MS = 125_000;
export const NONSTREAM_TIMEOUT_MS = 125_000;
