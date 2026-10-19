import * as os from 'node:os';
import * as fs from 'node:fs';
import * as path from 'node:path';
import {createHandler, HandlerContext, ToolError} from './tools/index.ts';

// Offline self-test for the client-side tools (no backend, no LLM).
// Exercises the tool dispatcher and every Handler against a throwaway
// workspace, proving the local execution half of the agent works.
//   node src/selftest.ts

async function main(): Promise<void> {
    const failures: string[] = [];
    const check = (label: string, cond: boolean) => {
        process.stdout.write((cond ? 'ok   ' : 'FAIL ') + ' ' + label + '\n');
        if (!cond) failures.push(label);
    };

    const ws = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-'));
    const ctx = new HandlerContext(ws);
    try {
        let r = await createHandler('write_file', ctx)!.execute({
            target_file: 'notes.txt',
            content: 'alpha\n# TODO: fix\n',
        });
        check('write_file creates a file', r.includes('Created') && fs.existsSync(path.join(ws, 'notes.txt')));

        r = await createHandler('read_file', ctx)!.execute({target_file: 'notes.txt'});
        check('read_file returns numbered lines', r.includes('1\u2192alpha'));

        r = await createHandler('list_dir', ctx)!.execute({target_directory: '.'});
        check('list_dir lists the new file', r.includes('notes.txt'));

        r = await createHandler('grep', ctx)!.execute({pattern: 'TODO'});
        check('grep finds the TODO', r.includes('notes.txt:2'));

        r = await createHandler('edit_file', ctx)!.execute({
            target_file: 'notes.txt',
            old_string: '# TODO: fix\n',
            new_string: '# fixed\n',
        });
        check('edit_file replaces unique string', r.includes('Edited'));
        r = await createHandler('read_file', ctx)!.execute({target_file: 'notes.txt'});
        check('edit_file took effect', r.includes('# fixed') && !r.includes('TODO'));

        r = await createHandler('run_command', ctx)!.execute({command: 'echo ran-ok'});
        check('run_command runs in workspace', r.includes('exit_code: 0') && r.includes('ran-ok'));

        let escaped = false;
        try {
            await createHandler('read_file', ctx)!.execute({target_file: '../../../etc/passwd'});
        } catch (e) {
            if (e instanceof ToolError) escaped = true;
        }
        check('path escape is rejected', escaped);

        let notfound = false;
        try {
            await createHandler('read_file', ctx)!.execute({target_file: 'nope.txt'});
        } catch (e) {
            if (e instanceof ToolError) notfound = true;
        }
        check('missing file raises ToolError', notfound);

        check('unknown tool -> null', createHandler('no_such_tool', ctx) === null);
    } finally {
        fs.rmSync(ws, {recursive: true, force: true});
    }

    process.stdout.write('\n');
    if (failures.length) {
        process.stdout.write(`${failures.length} check(s) failed\n`);
        process.exit(1);
    }
    process.stdout.write('all tool self-tests passed\n');
}

main().catch(e => {
    console.error(e);
    process.exit(1);
});

