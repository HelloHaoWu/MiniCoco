import {Handler, ToolError, stringArg, type ToolArgs} from './base.ts';

export class EditFile extends Handler {
    requiresApproval = true;
    name = 'edit_file';
    protected run(args: ToolArgs): string {
        const file = this.ctx.resolve(stringArg(args, 'target_file'));
        const oldString = stringArg(args, 'old_string');
        const newString = stringArg(args, 'new_string', undefined, true);
        const text = this.ctx.readText(file);
        const index = text.indexOf(oldString);
        if (index < 0) throw new ToolError('old_string not found in file');
        if (text.indexOf(oldString, index + 1) >= 0) throw new ToolError('old_string is not unique');
        this.ctx.writeText(file, text.slice(0, index) + newString + text.slice(index + oldString.length));
        return `Edited ${file}.`;
    }
}
