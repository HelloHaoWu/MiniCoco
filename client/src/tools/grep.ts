import * as fs from 'node:fs';
import * as path from 'node:path';
import {Handler, ToolError, type ToolArgs} from './base.ts';

// grep tool — mirrors Coco tools/GrepContent (simplified, pure JS walk).
const MAX_HITS = 200;
const SKIP = new Set(['.git', 'node_modules', '__pycache__', '.venv', 'dist', 'build']);

export class Grep extends Handler {
    name = 'grep';

    execute(args: ToolArgs): string {
        if (!args.pattern) {
            throw new ToolError("grep requires 'pattern'");
        }
        let rx: RegExp;
        try {
            rx = new RegExp(args.pattern);
        } catch (e) {
            throw new ToolError(`invalid regex: ${(e as Error).message}`);
        }
        const root = this.ctx.resolve(args.path);
        const files: string[] = [];
        const stat = fs.existsSync(root) ? fs.statSync(root) : null;
        if (stat?.isFile()) {
            files.push(root);
        } else if (stat?.isDirectory()) {
            this.walk(root, files);
        }

        const hits: string[] = [];
        for (const fp of files) {
            let content: string;
            try {
                content = fs.readFileSync(fp, 'utf8');
            } catch {
                continue;
            }
            const lines = content.split('\n');
            for (let i = 0; i < lines.length; i++) {
                if (rx.test(lines[i])) {
                    hits.push(`${path.relative(this.ctx.workspace, fp)}:${i + 1}:${lines[i]}`);
                    if (hits.length >= MAX_HITS) {
                        hits.push('... (truncated)');
                        return hits.join('\n');
                    }
                }
            }
        }
        return hits.length ? hits.join('\n') : 'No matches.';
    }

    walk(dir: string, out: string[]): void {
        for (const entry of fs.readdirSync(dir, {withFileTypes: true})) {
            if (entry.isDirectory()) {
                if (!SKIP.has(entry.name)) {
                    this.walk(path.join(dir, entry.name), out);
                }
            } else {
                out.push(path.join(dir, entry.name));
            }
        }
    }
}

