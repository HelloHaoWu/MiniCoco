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
