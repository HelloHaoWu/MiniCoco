import {readdirSync} from 'node:fs';
import {fileURLToPath} from 'node:url';
import {spawnSync} from 'node:child_process';

// Preserve process isolation between test files without the Node 24 test
// runner's mixed stdout/V8 framing bug (nodejs/node#64061, upstream #64706).
// Each independent process runs the native test harness directly, so product
// streaming output cannot corrupt an internal child-to-parent test channel.
const source = new URL('../src/', import.meta.url);
const files = readdirSync(source, {withFileTypes: true})
    .filter(entry => entry.isFile() && entry.name.endsWith('.test.ts'))
    .map(entry => entry.name).sort();
if (!files.length) throw new Error('No client test files found');
let failures = 0;
for (const file of files) {
    process.stdout.write(`\nClient tests: ${file}\n`);
    const result = spawnSync(process.execPath, ['--experimental-test-isolation=none', '--test', fileURLToPath(new URL(file, source))], {stdio: 'inherit'});
    if (result.error) throw result.error;
    if (result.status !== 0) failures++;
}
process.stdout.write(`\nClient test files: ${files.length}, failed: ${failures}\n`);
process.exitCode = failures ? 1 : 0;
