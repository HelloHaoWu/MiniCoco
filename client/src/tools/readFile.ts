import * as fs from 'node:fs';
import {Handler, ToolError, type ToolArgs} from './base.ts';

// read_file tool — mirrors Coco tools/ReadFile.
const MAX_LINES = 2000;

export class ReadFile extends Handler {
    name = 'read_file';

    execute(args: ToolArgs): string {
        const file = this.ctx.resolve(args.target_file);
        if (!fs.existsSync(file) || !fs.statSync(file).isFile()) {
            throw new ToolError(`file does not exist: ${file}`);
        }
        const raw = fs.readFileSync(file, 'utf8');
        if (raw === '') {
            return 'File is empty.';
        }
        const all = raw.replace(/\n$/, '').split('\n');
        const offset = Math.max(1, Number(args.offset ?? 1));
        const limit = Number(args.limit ?? MAX_LINES);
        const chunk = all.slice(offset - 1, offset - 1 + limit);
        return chunk.map((ln, i) => `${offset + i}\u2192${ln}`).join('\n');
    }
}

