import * as path from 'node:path';
import * as readline from 'node:readline';
import {Conversation} from './conversation.ts';
import {getSession, listSessions} from './sessions.ts';
import {realpathSync} from 'node:fs';

interface Args { query: string[]; workspace: string; workspaceGiven: boolean; server: string; model: string; maxRounds: number; resume: string; list: boolean; }

function parseArgs(argv: string[]): Args {
    const args: Args = {query: [], workspace: process.cwd(), workspaceGiven: false, server: process.env.MINICOCO_SERVER || 'http://127.0.0.1:8017', model: '', maxRounds: 25, resume: '', list: false};
    for (let i = 0; i < argv.length; i++) {
        const key = argv[i];
        if (key === '--') { args.query.push(...argv.slice(i + 1)); break; }
        if (key === '--list') {args.list = true;continue;}
        if (['--workspace', '--server', '--model', '--max-rounds', '--resume'].includes(key)) {
            const value = argv[++i];
            if (!value || value.startsWith('--')) throw new Error(`${key} requires a value`);
            if (key === '--workspace') {args.workspace = value;args.workspaceGiven = true;}
            if (key === '--server') args.server = value;
            if (key === '--model') args.model = value;
            if (key === '--max-rounds') args.maxRounds = Number(value);
            if (key === '--resume') args.resume = value;
        } else if (key.startsWith('--')) throw new Error(`unknown option: ${key}`);
        else args.query.push(key);
    }
    return args;
}

async function main(): Promise<void> {
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
    }
    const convo = new Conversation(args.server, path.resolve(args.workspace), args.model, args.maxRounds, {sessionId: args.resume, onSession: id => process.stderr.write(`\n[session] ${id}\n`)});
    process.stdout.write(`MiniCoco  server=${args.server}  model=${args.model || '(backend default)'}  workspace=${convo.workspace}\n`);
    let active: AbortController | undefined;
    let rl: readline.Interface | undefined;
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
        const terminal = !!process.stdin.isTTY && !!process.stdout.isTTY;
        rl = readline.createInterface({input: process.stdin, output: process.stdout, terminal, prompt: 'you> '});
        rl.on('close', () => { closed = true; });
        rl.on('SIGINT', interrupt);
        if (terminal) rl.prompt();
        for await (const raw of rl) {
            const query = raw.trim();
            if (['exit', 'quit'].includes(query.toLowerCase())) break;
            if (query) await run(query);
            if (terminal && !closed) rl.prompt();
        }
    } finally { rl?.close(); process.removeListener('SIGINT', interrupt); }
}

main().catch(error => { console.error(`[client] ${(error as Error).message}`); process.exitCode = 1; });
