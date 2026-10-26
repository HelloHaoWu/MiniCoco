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
        'need_compression' | 'bad_request' | 'upstream_timeout' | 'config' | 'internal' | 'storage';
    // Optional for older servers; absent metadata is conservatively fatal.
    retryable?: boolean;
}

export interface ToolCall {
    id: string;
    name: string;
    arguments: Record<string, unknown>;
}

// OpenAI-shaped assistant result; full conversation history lives on the server.
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

// Complete batch in execution/request order; the backend validates identities
// before selecting Native/XML presentation. Formatting never re-executes tools.
export interface ToolUseResult { id: string; name: string; output: string; isError: boolean; }
export type ToolMode = 'full' | 'readonly' | 'safe';
export interface RoundRequest {
    backgroundResults?: import('./background.ts').BackgroundResult[];
    parentSessionId?: string;
    agentType?: string;
    sessionId: string;
    roundId: string;
    query?: string;
    toolUseResults?: ToolUseResult[];
    workspace: string;
    model: string;
    mode?: ToolMode;
}
export interface Session {
    parentSessionId?: string | null; agentType?: string; forkedFrom?: string;
    id: string; user: string; model: string; workspace: string; created_at: number;
    ended: boolean; prompt_tokens: number; completion_tokens: number; total_tokens: number;
    message_count: number; last_message_count: number;
}

