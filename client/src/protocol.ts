// Wire protocol shared with the Go backend (see backend/protocol.go).
// Each SSE `data:` line is one Event. Mirrors Coco's CompletionStreamResult.

export const EV = {
    TEXT: 'text',
    REASONING: 'reasoning',
    TOOL_CALL: 'tool_call',
    DONE: 'done',
    ERROR: 'error',
} as const;

export const FINISH = {
    TOOL_CALLS: 'tool_calls', // model wants tools run -> client loops
    STOP: 'stop', // final answer -> loop ends
} as const;

export interface Event {
    type: string;
    delta?: string;
    id?: string;
    name?: string;
    arguments?: Record<string, unknown>;
    finish_reason?: string;
    message?: string;
    code?: 'upstream_5xx' | 'upstream_429' | 'rate_limited' | 'quota_exhausted' |
        'need_compression' | 'bad_request' | 'upstream_timeout' | 'config' | 'internal';
    // Optional for older servers. Part 1 keeps all errors fatal in the client;
    // Part 2 will use this metadata, conservatively treating missing as false.
    retryable?: boolean;
}

export interface ToolCall {
    id: string;
    name: string;
    arguments: Record<string, unknown>;
}

// OpenAI-shaped message the client maintains and replays each round.
export interface Message {
    role: string;
    content?: string | null;
    tool_calls?: Array<{
        id: string;
        type: 'function';
        function: {name: string; arguments: string};
    }>;
    tool_call_id?: string;
}

