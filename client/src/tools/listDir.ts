import * as fs from 'node:fs';
import * as path from 'node:path';
import {Handler, ToolError, type ToolArgs} from './base.ts';

// list_dir tool — mirrors Coco tools/ListDir.
export class ListDir extends Handler {
    name = 'list_dir';

    execute(args: ToolArgs): string {
        const dir = this.ctx.resolve(args.target_directory);
        if (!fs.existsSync(dir) || !fs.statSync(dir).isDirectory()) {
            throw new ToolError(`not a directory: ${dir}`);
        }
        const entries = fs.readdirSync(dir).sort();
        if (entries.length === 0) {
            return `${dir} is empty.`;
        }
        return entries
            .map(name => (fs.statSync(path.join(dir, name)).isDirectory() ? `${name}/` : name))
            .join('\n');
    }
}

