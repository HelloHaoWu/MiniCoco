import * as fs from 'node:fs';
import {Handler, ToolError, type ToolArgs} from './base.ts';

// edit_file tool — mirrors Coco tools/EditFile. Exact-string replacement:
// old_string must occur exactly once so the edit is unambiguous.
export class EditFile extends Handler {
    name = 'edit_file';

    execute(args: ToolArgs): string {
        const file = this.ctx.resolve(args.target_file);
        const oldStr = args.old_string;
        const newStr = args.new_string;
        if (oldStr === undefined || newStr === undefined) {
            throw new ToolError("edit_file requires 'old_string' and 'new_string'");
        }
        if (!fs.existsSync(file)) {
            throw new ToolError(`file does not exist: ${file}`);
        }
        const text = fs.readFileSync(file, 'utf8');
        const count = text.split(oldStr).length - 1;
        if (count === 0) {
            throw new ToolError('old_string not found in file');
        }
        if (count > 1) {
            throw new ToolError(`old_string is not unique (found ${count} times)`);
        }
        fs.writeFileSync(file, text.replace(oldStr, newStr));
        return `Edited ${file}.`;
    }
}

