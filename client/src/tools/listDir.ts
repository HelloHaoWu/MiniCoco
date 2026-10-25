import * as fs from 'node:fs';
import {Handler, stringArg, type ToolArgs} from './base.ts';

export class ListDir extends Handler {
    name = 'list_dir';
    protected run(args: ToolArgs): string {
        const dir = this.ctx.resolve(stringArg(args, 'target_directory', '.'));
        const entries: string[] = [];
        const handle = fs.opendirSync(dir);
        try {
            let entry: fs.Dirent | null;
            while ((entry = handle.readSync()) !== null) {
                if (entries.length >= 2000) { entries.push('... (entries truncated)'); break; }
                entries.push(entry.name + (entry.isSymbolicLink() ? '@' : entry.isDirectory() ? '/' : ''));
            }
        } finally { handle.closeSync(); }
        return entries.length ? entries.sort().join('\n') : `${dir} is empty.`;
    }
}
