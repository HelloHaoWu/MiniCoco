import type {ToolCall} from './protocol.ts';
import type {Handler} from './tools/base.ts';

export type ApprovalMode = 'interactive' | 'yes' | 'deny';
export type ApprovalPrompt = (call: ToolCall, signal?: AbortSignal) => Promise<boolean>;

export async function approve(handler: Handler, call: ToolCall, mode: ApprovalMode, prompt?: ApprovalPrompt): Promise<boolean> {
    handler.ctx.signal?.throwIfAborted();
    if (!handler.requiresApproval || mode === 'yes') return true;
    if (mode === 'deny') return false;
    return prompt ? prompt(call, handler.ctx.signal) : false;
}
