import * as path from 'node:path';
import * as readline from 'node:readline';
import {Conversation} from './conversation.ts';
import {getSession, listSessions} from './sessions.ts';
import {realpathSync} from 'node:fs';
import {normalizeMode} from './toolMode.ts';
import type {ToolMode} from './protocol.ts';
import {printTrace} from './trace.ts';

interface Args { query: string[]; workspace: string; workspaceGiven: boolean; server: string; model: string; maxRounds: number; resume: string; list: boolean; mode: ToolMode; yes: boolean; }

function parseArgs(argv: string[]): Args {
    const args: Args = {query: [], workspace: process.cwd(), workspaceGiven: false, server: process.env.MINICOCO_SERVER || 'http://127.0.0.1:8017', model: '', maxRounds: 25, resume: '', list: false, mode: 'full', yes: false};
    for (let i = 0; i < argv.length; i++) {
        const key = argv[i];
        if (key === '--') { args.query.push(...argv.slice(i + 1)); break; }
        if (key === '--list') {args.list = true;continue;}
        if (key === '--yes') {args.yes = true;continue;}
        if (['--workspace', '--server', '--model', '--max-rounds', '--resume', '--mode'].includes(key)) {
            const value = argv[++i];
            if (!value || value.startsWith('--')) throw new Error(`${key} requires a value`);
            if (key === '--workspace') {args.workspace = value;args.workspaceGiven = true;}
            if (key === '--server') args.server = value;
            if (key === '--model') args.model = value;
            if (key === '--max-rounds') args.maxRounds = Number(value);
            if (key === '--resume') args.resume = value;
            if (key === '--mode') args.mode = normalizeMode(value);
        } else if (key.startsWith('--')) throw new Error(`unknown option: ${key}`);
        else args.query.push(key);
    }
    return args;
}

async function main(): Promise<void> {
    if (process.argv[2] === 'trace') {
        const argv=process.argv.slice(3), sid=argv.shift() ?? '';
        let server=process.env.MINICOCO_SERVER || 'http://127.0.0.1:8017', round: number | undefined;
        while(argv.length) {const flag=argv.shift(), value=argv.shift();if (!value) throw new Error('trace option requires value');if(flag==='--round') round=Number(value);else if(flag==='--server') server=value;else throw new Error('unknown trace option');}
        await printTrace(server,sid,round);return;
    }
    const args = parseArgs(process.argv.slice(2));
    if (args.list) {
        if (args.resume || args.query.length) throw new Error('--list cannot be combined with --resume or a query');
        const sessions = await listSessions(args.server);
        process.stdout.write('SESSION\tMESSAGES\tTOKENS\tMODEL\tWORKSPACE\n');
        for (const session of sessions) process.stdout.write(`${session.id}\t${session.message_count}\t${session.total_tokens}\t${JSON.stringify(session.model)}\t${JSON.stringify(session.workspace)}\n`);
        return;
    }
    if (args.resume) {
        const {session, canResume} = await getSession(args.server, args.resume);
        if (!canResume) throw new Error('session has an unfinished tool batch; refusing to replay tools whose side effects may already exist');
        if (args.workspaceGiven && realpathSync(path.resolve(args.workspace)) !== session.workspace) throw new Error('--workspace differs from the saved session workspace');
        args.workspace = session.workspace;
        if (!args.model) args.model = session.model;
        if (session.agentType === 'explore') args.mode = 'readonly';
    }
    const terminal = !!process.stdin.isTTY && !!process.stdout.isTTY;
    let rl: readline.Interface | undefined;
    const question = (text: string, signal?: AbortSignal): Promise<string> => new Promise((resolve, reject) => {
        const input = rl ?? readline.createInterface({input: process.stdin, output: process.stdout, terminal});
        const own = !rl;
        const cleanup = (): void => {input.removeListener('close', close);signal?.removeEventListener('abort', abort);if (own) {input.removeListener('SIGINT', interrupt);input.close();}};
        const close = (): void => {cleanup();resolve('');};
        const abort = (): void => {cleanup();reject(signal?.reason ?? new Error('cancelled'));};
        input.once('close', close);signal?.addEventListener('abort', abort, {once: true});
        if (own) input.on('SIGINT', interrupt);
        if (signal?.aborted) {abort();return;}
        input.question(text, {signal}, answer => {cleanup();resolve(answer);});
    });
    const convo = new Conversation(args.server, path.resolve(args.workspace), args.model, args.maxRounds, {mode: args.mode, sessionId: args.resume, approvalMode: args.yes ? 'yes' : terminal ? 'interactive' : 'deny', approvalPrompt: async (call, signal) => /^(y|yes)$/i.test((await question(`执行 ${call.name} ${JSON.stringify(call.arguments).slice(0,240)} ? [y/N] `, signal)).trim()), onSession: id => process.stderr.write(`\n[session] ${id}\n`)});
    process.stdout.write(`MiniCoco  server=${args.server}  model=${args.model || '(backend default)'}  mode=${args.mode}  workspace=${convo.workspace}\n`);
    let active: AbortController | undefined;
    let closed = false;
    const interrupt = (): void => {
        if (active) active.abort(new Error('cancelled by user'));
        else if (rl) { closed = true; rl.close(); }
    };
    process.on('SIGINT', interrupt);
    const run = async (query: string): Promise<void> => {
        active = new AbortController();
        try {
            const result = await convo.run(query, active.signal);
            if (result.status === 'completed') process.stdout.write(`\n=== answer ===\n${result.text}\n\n`);
            else {
                process.stderr.write(`\n[${result.status}] ${result.message}\n`);
                process.exitCode = result.status === 'cancelled' ? 130 : 1;
            }
        } finally { active = undefined; }
    };
    try {
        if (args.query.length) { await run(args.query.join(' ')); return; }
        rl = readline.createInterface({input: process.stdin, output: process.stdout, terminal, prompt: 'you> '});
        rl.on('close', () => { closed = true; });
        rl.on('SIGINT', interrupt);
        if (terminal) {
            while (!closed) {const query = (await question('you> ')).trim();if (['exit','quit'].includes(query.toLowerCase())) break;if (query) await run(query);}
            return;
        }
        for await (const raw of rl) {
            const query = raw.trim();
            if (['exit', 'quit'].includes(query.toLowerCase())) break;
            if (query) await run(query);
            if (terminal && !closed) rl.prompt();
        }
    } finally { rl?.close(); process.removeListener('SIGINT', interrupt); await convo.close(); }
}

main().catch(error => { console.error(`[client] ${(error as Error).message}`); process.exitCode = 1; });
