import {Handler, stringArg, integerArg, type ToolArgs} from './base.ts';

export class ReadFile extends Handler {
    name = 'read_file';
    protected run(args: ToolArgs): string {
        const file = this.ctx.resolve(stringArg(args, 'target_file'));
        const offset = integerArg(args, 'offset', 1, Number.MAX_SAFE_INTEGER);
        const limit = integerArg(args, 'limit', 2000, 2000);
        const raw = this.ctx.readText(file);
        if (raw === '') return 'File is empty.';
        return raw.replace(/\n$/, '').split('\n').slice(offset - 1, offset - 1 + limit)
            .map((line, i) => `${offset + i}→${line}`).join('\n');
    }
}
