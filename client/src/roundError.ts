export type RoundErrorKind = 'network' | 'timeout' | 'http' | 'backend' | 'protocol' | 'abort';

export class RoundError extends Error {
    kind: RoundErrorKind;
    httpStatus?: number;
    code?: string;
    retryable?: boolean;
    attempts?: number;
    traceId?: string;

    constructor(message: string, kind: RoundErrorKind, details: {httpStatus?: number; code?: string; retryable?: boolean; cause?: unknown} = {}) {
        super(message, {cause: details.cause});
        this.name = 'RoundError';
        this.kind = kind;
        this.httpStatus = details.httpStatus;
        this.code = details.code;
        this.retryable = details.retryable;
    }
}

export function abortError(signal?: AbortSignal): RoundError {
    return new RoundError('cancelled by user', 'abort', {cause: signal?.reason});
}
