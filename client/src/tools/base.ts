import * as path from 'node:path';

// Tool handler base + shared helpers.
//
// Mirrors Coco's tools/Handler.ts + Tool.ts: every tool is a Handler that
// validates its args and runs locally on the user's machine, returning a result
// string the client sends back to the backend as a tool message.

export class ToolError extends Error {}

export type ToolArgs = Record<string, any>;

export class HandlerContext {
    workspace: string;

    constructor(workspace: string) {
        this.workspace = path.resolve(workspace);
    }

    // Resolve a model-supplied path against the workspace root, rejecting paths
    // that escape it (keeps tool execution sandboxed to the workspace).
    resolve(p?: string): string {
        if (!p) {
            return this.workspace;
        }
        const candidate = path.resolve(this.workspace, p);
        if (candidate !== this.workspace && !candidate.startsWith(this.workspace + path.sep)) {
            throw new ToolError(`path escapes workspace root: ${p}`);
        }
        return candidate;
    }
}

export class Handler {
    ctx: HandlerContext;
    name = 'base';

    constructor(ctx: HandlerContext) {
        this.ctx = ctx;
    }

    execute(_args: ToolArgs): Promise<string> | string {
        throw new Error('not implemented');
    }
}

