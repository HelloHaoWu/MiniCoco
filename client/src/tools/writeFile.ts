import * as fs from 'node:fs';
import {Handler, stringArg, type ToolArgs} from './base.ts';

export class WriteFile extends Handler {
    name = 'write_file';
    protected run(args: ToolArgs): string {
        const file = this.ctx.resolve(stringArg(args, 'target_file'));
        const content = stringArg(args, 'content', undefined, true);
        const existed = fs.existsSync(file);
        this.ctx.writeText(file, content);
        return `${existed ? 'Overwrote' : 'Created'} ${file} (${content.split('\n').length} lines).`;
    }
}
