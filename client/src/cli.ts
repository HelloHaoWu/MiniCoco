import * as fs from 'node:fs';
import * as path from 'node:path';
import * as readline from 'node:readline';
import {Conversation} from './conversation.ts';

// CLI entrypoint for the MiniCoco client.
//
// Mirrors the role of the IDE/webview front-end: collect a user query, hand it
// to the Conversation (the agent loop), and render the streamed result. Run
// with a one-shot query, or with no query for an interactive REPL.
//
//   node src/cli.ts "list the files here" --workspace /path/to/project
//   node src/cli.ts --workspace /path/to/project      # interactive

const DEFAULT_SERVER = process.env.MINICOCO_SERVER || 'http://127.0.0.1:8017';
const BOLD = '\x1b[1m';
const RESET = '\x1b[0m';
const CYAN = '\x1b[36m';

interface Args {
    query: string[];
    workspace: string;
    server: string;
    model: string;
    maxRounds: number;
}

function parseArgs(argv: string[]): Args {
    const a: Args = {query: [], workspace: process.cwd(), server: DEFAULT_SERVER, model: '', maxRounds: 25};
    for (let i = 0; i < argv.length; i++) {
        const t = argv[i];
        if (t === '--workspace') a.workspace = argv[++i];
        else if (t === '--server') a.server = argv[++i];
        else if (t === '--model') a.model = argv[++i];
        else if (t === '--max-rounds') a.maxRounds = Number(argv[++i]);
        else a.query.push(t);
    }
    return a;
}

async function runTurn(convo: Conversation, query: string): Promise<void> {
    let finalText: string;
    try {
        finalText = await convo.run(query);
    } catch (e) {
        process.stderr.write(`\n[client] cannot reach backend at ${convo.serverUrl}: ${(e as Error).message}\n`);
        process.stderr.write('[client] is the backend running?  ./run_backend.sh\n');
        return;
    }
    process.stdout.write(`\n${BOLD}=== answer ===${RESET}\n${finalText}\n\n`);
}

function modelLabel(model: string): string {
    return model || '(backend default)';
}

async function main(): Promise<void> {
    const a = parseArgs(process.argv.slice(2));
    const workspace = path.resolve(a.workspace);
    if (!fs.existsSync(workspace) || !fs.statSync(workspace).isDirectory()) {
        process.stderr.write(`workspace is not a directory: ${workspace}\n`);
        process.exit(2);
    }
    process.stdout.write(`${BOLD}MiniCoco${RESET}  server=${a.server}  model=${modelLabel(a.model)}  workspace=${workspace}\n`);

    // One Conversation reused across turns, so interactive mode keeps history.
    const convo = new Conversation(a.server, workspace, a.model, a.maxRounds);

    if (a.query.length) {
        await runTurn(convo, a.query.join(' '));
        return;
    }

    process.stdout.write("Interactive mode. Type a task, or 'exit' to quit.\n");
    const rl = readline.createInterface({
        input: process.stdin,
        output: process.stdout,
        prompt: `${CYAN}you>${RESET} `,
    });
    rl.prompt();
    // Consume readline as an async iterator: every incoming line is queued, so
    // awaiting a long turn never drops input. Piped input (printf '...' |
    // run_client.sh) now runs every turn, and a real TTY still waits per line.
    for await (const rawLine of rl) {
        const line = rawLine.trim();
        const low = line.toLowerCase();
        if (low === 'exit' || low === 'quit') break;
        if (line) await runTurn(convo, line);
        rl.prompt();
    }
    rl.close();
}

main().catch(e => {
    console.error(e);
    process.exit(1);
});

