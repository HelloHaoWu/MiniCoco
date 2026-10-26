import {Handler, ToolError, stringArg, type ToolArgs} from './base.ts';

export class DelegateSubagent extends Handler {
    name = 'delegate_subagent';
    protected run(args: ToolArgs): Promise<string> {
        stringArg(args, 'description');stringArg(args, 'agent_type');stringArg(args, 'query');
        if (!this.ctx.delegate) throw new ToolError('subagent execution unavailable');
        return this.ctx.delegate(args);
    }
}
