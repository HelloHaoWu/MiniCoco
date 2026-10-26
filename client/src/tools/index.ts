import {Handler, HandlerContext, ToolError} from './base.ts';
import {ReadFile} from './readFile.ts';
import {WriteFile} from './writeFile.ts';
import {EditFile} from './editFile.ts';
import {ListDir} from './listDir.ts';
import {Grep} from './grep.ts';
import {RunCommand} from './runCommand.ts';
import {DelegateSubagent} from './delegateSubagent.ts';

// Tool dispatch registry.
//
// Mirrors Coco's tools/createHandler.ts: map a tool name coming from the
// model to the concrete local Handler that executes it.
type HandlerCtor = new (ctx: HandlerContext) => Handler;

const REGISTRY: Record<string, HandlerCtor> = {
    read_file: ReadFile,
    list_dir: ListDir,
    grep: Grep,
    write_file: WriteFile,
    edit_file: EditFile,
    run_command: RunCommand,
    delegate_subagent: DelegateSubagent,
};

export function createHandler(name: string, ctx: HandlerContext): Handler | null {
    const Ctor = Object.hasOwn(REGISTRY, name) ? REGISTRY[name] : undefined;
    return Ctor ? new Ctor(ctx) : ctx.mcp?.handler(name, ctx) ?? null;
}

export {Handler, HandlerContext, ToolError};

