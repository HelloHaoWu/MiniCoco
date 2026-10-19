import * as fs from 'node:fs';
import * as path from 'node:path';
import {Handler, ToolError, type ToolArgs} from './base.ts';

// write_file tool — mirrors Coco tools/WriteFile.
export class WriteFile extends Handler {
    name = 'write_file';

    execute(args: ToolArgs): string {
        const file = this.ctx.resolve(args.target_file);
        if (args.content === undefined || args.content === null) {
            throw new ToolError("write_file requires 'content'");
        }
        const content = String(args.content);
        const existed = fs.existsSync(file);
        fs.mkdirSync(path.dirname(file), {recursive: true});
        fs.writeFileSync(file, content);
        const n = content.split('\n').length;
        return `${existed ? 'Overwrote' : 'Created'} ${file} (${n} lines).`;
    }
}

