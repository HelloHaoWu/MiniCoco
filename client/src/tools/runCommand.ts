import {spawnSync} from 'node:child_process';
import {Handler, ToolError, type ToolArgs} from './base.ts';

// run_command tool — mirrors Coco tools/RunCommand.
//
// Runs a non-interactive shell command inside the workspace with a timeout.
// This is the sharpest tool in a coding agent: it executes arbitrary commands
// on the user's machine. The harness keeps it simple (cwd=workspace, timeout)
// and relies on the localhost-only, developer-run nature of the server.
const TIMEOUT_MS = 120_000;

export class RunCommand extends Handler {
    name = 'run_command';

    execute(args: ToolArgs): string {
        if (!args.command) {
            throw new ToolError("run_command requires 'command'");
        }
        const res = spawnSync(String(args.command), {
            cwd: this.ctx.workspace,
            shell: true,
            encoding: 'utf8',
            timeout: TIMEOUT_MS,
            maxBuffer: 10 * 1024 * 1024,
        });
        if (res.error) {
            if ((res.error as NodeJS.ErrnoException).code === 'ETIMEDOUT') {
                throw new ToolError(`command timed out after ${TIMEOUT_MS / 1000}s`);
            }
            throw new ToolError(res.error.message);
        }
        const parts = [`exit_code: ${res.status ?? 'null'}`];
        if (res.stdout) {
            parts.push('stdout:\n' + res.stdout.trimEnd());
        }
        if (res.stderr) {
            parts.push('stderr:\n' + res.stderr.trimEnd());
        }
        return parts.join('\n');
    }
}

