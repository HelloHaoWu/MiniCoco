import {test} from 'node:test';
import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import * as http from 'node:http';
import {once} from 'node:events';
import {spawn, spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {getSession, listSessions} from './sessions.ts';
import {runRoundWithRetry} from './retry.ts';
import {RoundError} from './roundError.ts';

const project = fileURLToPath(new URL('../..', import.meta.url));

async function cli(args: string[]): Promise<{code: number | null; stdout: string; stderr: string}> {
    const child = spawn(process.execPath, [path.join(project, 'client/src/cli.ts'), ...args], {stdio: ['pipe', 'pipe', 'pipe']});
    let stdout = '', stderr = '';
    child.stdout.on('data', chunk => {stdout += chunk;});child.stderr.on('data', chunk => {stderr += chunk;});
    const closed = once(child, 'close');child.stdin.end();const [code] = await closed;
    return {code, stdout, stderr};
}

async function backend(binary: string, dir: string, model: string, loop = '', upstream = ''): Promise<{url: string; stop: () => Promise<void>}> {
    const reservation = http.createServer();reservation.listen(0, '127.0.0.1');await once(reservation, 'listening');
    const port = (reservation.address() as import('node:net').AddressInfo).port;
    await new Promise<void>(resolve => reservation.close(() => resolve()));
    const child = spawn(binary, [], {cwd: dir, env: {...process.env, STORE_BACKEND: 'file', MINICOCO_DATA_DIR: path.join(dir, 'data'),
        LOOP_IMPL: loop, MINICOCO_MODEL_IS_FUNCTION_CALL: '', MINICOCO_MODELS_FILE: '', LLM_MODEL: model, MINICOCO_DEBUG_PROMPT: '0', LLM_PROVIDER: upstream ? 'openai' : 'mock', LLM_BASE_URL: upstream, LLM_API_KEY: upstream ? 'fake-test-key' : '', LLM_MOCK_MODE: 'normal',
        MINICOCO_HOST: '127.0.0.1', MINICOCO_PORT: String(port)}, stdio: 'ignore'});
    const exited = once(child, 'exit');
    const stop = async (): Promise<void> => {if (child.exitCode === null) child.kill('SIGTERM');await exited;};
    const url = `http://127.0.0.1:${port}`;
    for (let i = 0; i < 100; i++) {
        try {if ((await fetch(url + '/health')).ok) return {url, stop};} catch {}
        if (child.exitCode !== null) break;
        await new Promise(resolve => setTimeout(resolve, 20));
    }
    await stop();throw new Error('formatter backend did not start');
}

test('routed Native/XML and Claude-family mocks persist/resume; unknown models are fatal', {timeout: 30_000}, async () => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-format-cli-'));
    try {
        const binary = path.join(temp, 'backend');
        const build = spawnSync('go', ['build', '-o', binary, '.'], {cwd: path.join(project, 'backend'), encoding: 'utf8'});
        assert.equal(build.status, 0, build.stderr);
        for (const model of ['mock', 'mock-xml', 'mock-claude']) {
            const native = model !== 'mock-xml';
            const dir = path.join(temp, model);fs.mkdirSync(dir);
            const workspace = path.join(dir, 'workspace');fs.mkdirSync(workspace);
            let running = await backend(binary, dir, model);
            try {
                const first = await cli(['inspect', '--server', running.url, '--workspace', workspace, '--mode', 'readonly']);
                assert.equal(first.code, 0, first.stderr);assert.match(first.stdout, /Done\. I inspected/);
                const sid = first.stderr.match(/\[session\] (s_[a-f0-9]+)/)?.[1];assert(sid, first.stderr);
                const transcript = path.join(dir, 'data/transcripts', sid + '.jsonl');
                const rows = fs.readFileSync(transcript, 'utf8').trim().split('\n').map(line => JSON.parse(line) as {role: string; content?: string; request_hash?: string});
                const inputs = rows.filter(row => !row.request_hash);
                if (native) assert(inputs.some(row => row.role === 'tool'));
                else {
                    assert(inputs.some(row => row.role === 'user' && row.content?.startsWith('<tool_result><status>succeeded</status>')));
                    assert(!inputs.some(row => row.role === 'tool'));
                }
                const info = await getSession(running.url, sid);
                assert.equal(info.session.message_count, 4);assert.equal(info.session.model, model);
                await assert.rejects(() => runRoundWithRetry({serverUrl: running.url, sessionId: '', roundId: 'unknown-model', query: 'inspect', workspace, model: 'unknown'}, undefined, undefined, {onRetry: () => assert.fail('unknown model retried')}),
                    (error: unknown) => error instanceof RoundError && error.httpStatus === 400 && error.attempts === 1);
                assert.equal((await listSessions(running.url)).length, 1, 'unknown model created a session');
                await running.stop();running = await backend(binary, dir, model);
                const resumed = await cli(['again', '--server', running.url, '--resume', sid, '--mode', 'readonly']);
                assert.equal(resumed.code, 0, resumed.stderr);
                assert.equal((await getSession(running.url, sid)).session.message_count, 8);
                const listed = await cli(['--list', '--server', running.url]);assert.equal(listed.code, 0, listed.stderr);assert.match(listed.stdout, new RegExp(sid));
            } finally {await running.stop();}
        }
    } finally {fs.rmSync(temp, {recursive: true, force: true});}
});

test('default, explicit v1 and unregistered loop selections complete the real mock CLI', {timeout: 30_000}, async () => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-loop-cli-'));
    try {
        const binary = path.join(temp, 'backend');
        const build = spawnSync('go', ['build', '-o', binary, '.'], {cwd: path.join(project, 'backend'), encoding: 'utf8'});
        assert.equal(build.status, 0, build.stderr);
        for (const loop of ['', 'v1', 'unregistered']) {
            const dir = path.join(temp, loop || 'default');fs.mkdirSync(dir);
            const workspace = path.join(dir, 'workspace');fs.mkdirSync(workspace);
            const running = await backend(binary, dir, 'mock', loop);
            try {
                const health = await (await fetch(running.url + '/health')).json() as {loop_impl: string};
                assert.equal(health.loop_impl, 'v1');
                const result = await cli(['inspect', '--server', running.url, '--workspace', workspace, '--mode', 'readonly']);
                assert.equal(result.code, 0, result.stderr);assert.match(result.stdout, /Done\. I inspected/);
                const sid = result.stderr.match(/\[session\] (s_[a-f0-9]+)/)?.[1];assert(sid, result.stderr);
                const info = await getSession(running.url, sid);
                assert.equal(info.session.message_count, 4);assert.equal(info.session.total_tokens, 4);
            } finally {await running.stop();}
        }
    } finally {fs.rmSync(temp, {recursive: true, force: true});}
});

test('v2 compresses long CLI sessions and resumes checkpoints after backend restart', {timeout: 30_000}, async () => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), 'minicoco-compress-cli-'));
    try {
        const binary = path.join(temp, 'backend');
        const build = spawnSync('go', ['build', '-o', binary, '.'], {cwd: path.join(project, 'backend'), encoding: 'utf8'});
        assert.equal(build.status, 0, build.stderr);
        const workspace = path.join(temp, 'workspace');fs.mkdirSync(workspace);
        let running = await backend(binary, temp, 'mock', 'v2');
        try {
            const query = 'remember-this-path project/src/main.go ' + 'x'.repeat(24_000);
            const first = await cli([query, '--server', running.url, '--workspace', workspace]);
            assert.equal(first.code, 0, first.stderr);
            const sid = first.stderr.match(/\[session\] (s_[a-f0-9]+)/)?.[1];assert(sid, first.stderr);
            const second = await cli([query, '--resume', sid, '--server', running.url]);
            assert.equal(second.code, 0, second.stderr);
            const summaryPath = path.join(temp, 'data/transcripts', sid + '.summary.jsonl');
            const summaries = fs.readFileSync(summaryPath, 'utf8').trim().split('\n').map(line => JSON.parse(line) as {prefix_count: number; content: string});
            assert(summaries.some(summary => summary.prefix_count >= 3 && summary.content.includes('Mock context summary')));
            const canonical = fs.readFileSync(path.join(temp, 'data/transcripts', sid + '.jsonl'), 'utf8');
            assert(canonical.includes('remember-this-path project/src/main.go'), 'compression erased the original transcript');
            await running.stop();running = await backend(binary, temp, 'mock', 'v2');
            const third = await cli(['continue after restart', '--resume', sid, '--server', running.url]);
            assert.equal(third.code, 0, third.stderr);
            const info = await getSession(running.url, sid);
            assert.equal(info.session.message_count, 12);assert.equal(info.session.total_tokens, 12);
            assert.equal((await (await fetch(running.url + '/health')).json() as {loop_impl: string}).loop_impl, 'v2');
        } finally {await running.stop();}
    } finally {fs.rmSync(temp, {recursive: true, force: true});}
});

test('subagent has a persisted readonly child session and returns one final result to parent', {timeout:30_000}, async()=>{
    const temp=fs.mkdtempSync(path.join(os.tmpdir(),'minicoco-subagent-'));
    const received: {child:boolean;messages:{role:string;content?:string;tool_calls?:{function:{name:string}}[]}[]}[]=[];
    const upstream=http.createServer(async(req,res)=>{
        let raw='';for await(const chunk of req)raw+=chunk;const input=JSON.parse(raw);
        const child=String(input.messages[0].content).startsWith('You are a read-only exploration subagent.');
        received.push({child,messages:input.messages});
        if(child)assert.deepEqual(input.tools.map((tool: {function:{name:string}})=>tool.function.name),['read_file','list_dir','grep']);
        const last=input.messages.at(-1);let delta:unknown,finish='tool_calls';
        const exhaust = child && input.messages.some((m: {content?:string}) => m.content === 'loop');
        if(last.role==='tool' && !exhaust){delta={content:child?'findings: main.go:1':'parent finished: '+last.content};finish='stop';}
        else {const name=child?'list_dir':'delegate_subagent';const args=child?{}:{description:'find Go files',agent_type:'explore',query:String(last.content).includes('exhaust')?'loop':'list Go files'};delta={tool_calls:[{index:0,id:child?'child-list':'parent-delegate',function:{name,arguments:JSON.stringify(args)}}]};}
        res.setHeader('Content-Type','text/event-stream');res.end('data: '+JSON.stringify({choices:[{delta,finish_reason:finish}],usage:{prompt_tokens:1,completion_tokens:1,total_tokens:2}})+'\n\ndata: [DONE]\n\n');
    });
    upstream.listen(0,'127.0.0.1');await once(upstream,'listening');
    try{
        const binary=path.join(temp,'backend');const build=spawnSync('go',['build','-o',binary,'.'],{cwd:path.join(project,'backend'),encoding:'utf8'});assert.equal(build.status,0,build.stderr);
        const ws=path.join(temp,'workspace');fs.mkdirSync(ws);fs.writeFileSync(path.join(ws,'main.go'),'package main\n');
        const url=`http://127.0.0.1:${(upstream.address() as {port:number}).port}`;
        let running=await backend(binary,temp,'deepseek-flash','',url);
        try{
            const result=await cli(['delegate','--server',running.url,'--workspace',ws]);assert.equal(result.code,0,result.stderr);assert.match(result.stdout,/parent finished: findings: main.go:1/);
            const sessions=await listSessions(running.url);assert.equal(sessions.length,2);
            const child=sessions.find(s=>s.agentType==='explore')!;const parent=sessions.find(s=>!s.agentType)!;
            assert.equal(child.parentSessionId,parent.id);assert.equal(child.message_count,4);assert.equal(parent.message_count,4);
            const parentPrompt=received.filter(r=>!r.child).at(-1)!.messages;
            assert.equal(parentPrompt.filter(m=>m.role==='tool').length,1);assert(!JSON.stringify(parentPrompt).includes('child-list'));
            await running.stop();running=await backend(binary,temp,'deepseek-flash','',url);
            assert.equal((await getSession(running.url,child.id)).session.parentSessionId,parent.id);
            const exhausted = await cli(['exhaust delegate','--server',running.url,'--workspace',ws]);assert.equal(exhausted.code,0,exhausted.stderr);assert.match(exhausted.stdout,/子任务未完成（exhausted）/);
            const exhaustedChild = (await listSessions(running.url)).find(s=>s.agentType==='explore' && s.id!==child.id)!;assert.equal(exhaustedChild.message_count,20);assert.equal((await getSession(running.url,exhaustedChild.id)).canResume,false);
        }finally{await running.stop();}
    }finally{upstream.closeAllConnections();await new Promise<void>(resolve=>upstream.close(()=>resolve()));fs.rmSync(temp,{recursive:true,force:true});}
});

test('real CLI waits for background completion and backend injects task_complete in the same session', {timeout:30_000}, async()=>{
    const temp=fs.mkdtempSync(path.join(os.tmpdir(),'minicoco-bg-cli-'));let sawComplete=false,calls=0;
    const upstream=http.createServer(async(req,res)=>{
        let raw='';for await(const chunk of req)raw+=chunk;const input=JSON.parse(raw);calls++;
        const history=JSON.stringify(input.messages);sawComplete ||= history.includes('task_complete')&&history.includes('[background ')&&history.includes('bg-done');
        const last=input.messages.at(-1);let delta:unknown,finish='stop';
        if(calls===1){delta={tool_calls:[{index:0,id:'background',function:{name:'run_command',arguments:JSON.stringify({command:'sleep 0.15; printf x >> count; echo bg-done',run_in_background:true})}}]};finish='tool_calls';}
        else delta={content:sawComplete?'consumed background result':'waiting for task'};
        res.setHeader('Content-Type','text/event-stream');res.end('data: '+JSON.stringify({choices:[{delta,finish_reason:finish}]})+'\n\ndata: [DONE]\n\n');
    });upstream.listen(0,'127.0.0.1');await once(upstream,'listening');
    try{
        const binary=path.join(temp,'backend');const build=spawnSync('go',['build','-o',binary,'.'],{cwd:path.join(project,'backend'),encoding:'utf8'});assert.equal(build.status,0,build.stderr);
        const ws=path.join(temp,'workspace');fs.mkdirSync(ws);const url=`http://127.0.0.1:${(upstream.address() as {port:number}).port}`;
        const running=await backend(binary,temp,'deepseek-flash','',url);
        try{const result=await cli(['background work','--yes','--wait-background','--workspace',ws,'--server',running.url]);assert.equal(result.code,0,result.stderr);assert.match(result.stdout,/taskId=/);assert.match(result.stdout,/consumed background result/);assert(sawComplete);assert.equal(fs.readFileSync(path.join(ws,'count'),'utf8'),'x');assert.equal((await listSessions(running.url)).length,1);}
        finally{await running.stop();}
    }finally{upstream.closeAllConnections();await new Promise<void>(resolve=>upstream.close(()=>resolve()));fs.rmSync(temp,{recursive:true,force:true});}
});

test('CLI forks a committed assistant point, preserves source bytes and resumes the independent branch', {timeout:30_000}, async()=>{
    const temp=fs.mkdtempSync(path.join(os.tmpdir(),'minicoco-fork-cli-'));
    try{
        const binary=path.join(temp,'backend');const build=spawnSync('go',['build','-o',binary,'.'],{cwd:path.join(project,'backend'),encoding:'utf8'});assert.equal(build.status,0,build.stderr);
        const ws=path.join(temp,'workspace');fs.mkdirSync(ws);const running=await backend(binary,temp,'mock');
        try{
            const initial=await cli(['first','--workspace',ws,'--server',running.url]);assert.equal(initial.code,0,initial.stderr);
            const source=(await listSessions(running.url))[0];
            for(const query of ['second','third'])assert.equal((await cli([query,'--resume',source.id,'--server',running.url])).code,0);
            const sourceFile=path.join(temp,'data/transcripts',source.id+'.jsonl');const before=fs.readFileSync(sourceFile);
            const fork=await cli(['--fork','--resume',source.id,'--message','assistant:2','--server',running.url]);assert.equal(fork.code,0,fork.stderr);
            const result=JSON.parse(fork.stdout);assert.equal(result.resolvedForkMessageId,'message:4');assert.deepEqual(fs.readFileSync(sourceFile),before);
            const child=(await getSession(running.url,result.newSessionId)).session;assert.equal(child.parentSessionId,null);assert.equal(child.forkedFrom,source.id);assert.equal(child.message_count,4);assert.equal(child.total_tokens,0);
            assert.equal((await cli(['branch work','--resume',child.id,'--server',running.url])).code,0);
            assert.equal((await getSession(running.url,child.id)).session.message_count,8);assert.deepEqual(fs.readFileSync(sourceFile),before);
            assert.notEqual((await cli(['query','--fork','--resume',source.id,'--server',running.url])).code,0);
        }finally{await running.stop();}
    }finally{fs.rmSync(temp,{recursive:true,force:true});}
});

test('real CLI discovers stdio MCP, sends dynamic schema and persists returned tool output', {timeout:30_000}, async()=>{
    const temp=fs.mkdtempSync(path.join(os.tmpdir(),'minicoco-mcp-cli-'));
    let running: Awaited<ReturnType<typeof backend>> | undefined;
    let rounds=0;
    const upstream=http.createServer(async(req,res)=>{
        let raw='';for await(const chunk of req)raw+=chunk;
        const body=JSON.parse(raw);rounds++;
        assert.ok(body.tools.some((t:{function:{name:string}})=>t.function.name==='mcp__echo__ping'));
        res.writeHead(200,{'Content-Type':'text/event-stream'});
        if(rounds===1)res.end('data: '+JSON.stringify({choices:[{delta:{tool_calls:[{index:0,id:'echo-call',type:'function',function:{name:'mcp__echo__ping',arguments:'{"msg":"from-mcp"}'}}]},finish_reason:'tool_calls'}]})+'\n\ndata: [DONE]\n\n');
        else {assert.ok(body.messages.some((m:{role:string;content:string})=>m.role==='tool'&&m.content==='from-mcp'));res.end('data: '+JSON.stringify({choices:[{delta:{content:'MCP done'},finish_reason:'stop'}]})+'\n\ndata: [DONE]\n\n');}
    });
    try {
        const script=path.join(temp,'echo.cjs');fs.writeFileSync(script,`require('node:readline').createInterface({input:process.stdin}).on('line',l=>{const m=JSON.parse(l);if(!m.id)return;const result=m.method==='initialize'?{protocolVersion:'2024-11-05',capabilities:{tools:{}}}:m.method==='tools/list'?{tools:[{name:'ping',inputSchema:{type:'object',properties:{msg:{type:'string'}}}}]}:{content:[{type:'text',text:m.params.arguments.msg}]};process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:m.id,result})+'\\n');});`);
        const config=path.join(temp,'mcp.json');fs.writeFileSync(config,JSON.stringify({servers:[{name:'echo',transport:'stdio',command:process.execPath,args:[script],readOnly:true}]}));
        upstream.listen(0,'127.0.0.1');await once(upstream,'listening');
        const binary=path.join(temp,'backend'),build=spawnSync('go',['build','-o',binary,'.'],{cwd:path.join(project,'backend'),encoding:'utf8'});assert.equal(build.status,0,build.stderr);
        running=await backend(binary,temp,'deepseek-flash','v2',`http://127.0.0.1:${(upstream.address() as import('node:net').AddressInfo).port}`);
        const result=await cli(['echo','--workspace',temp,'--server',running.url,'--mcp-config',config,'--mode','readonly']);
        assert.equal(result.code,0,result.stderr);assert.match(result.stdout,/MCP done/);assert.equal(rounds,2);
    } finally {await running?.stop();await new Promise<void>(resolve=>upstream.close(()=>resolve()));fs.rmSync(temp,{recursive:true,force:true});}
});

test('CLI memory persists across sessions, injects only index, reads entries/code and audits approval', {timeout:30_000}, async()=>{
    const temp=fs.mkdtempSync(path.join(os.tmpdir(),'minicoco-memory-cli-'));
    let running: Awaited<ReturnType<typeof backend>> | undefined;
    let rounds=0,index='',entry='';
    const upstream=http.createServer(async(req,res)=>{
        let raw='';for await(const chunk of req)raw+=chunk;const body=JSON.parse(raw);rounds++;
        assert.match(body.messages[0].content,/# Memory/);assert.equal(body.messages[1].role,'system');assert.ok(body.messages[1].content.includes('<MEMORY.md>\n'+index+'\n</MEMORY.md>'));assert.equal(body.messages[2].role,'user');
        if(rounds===1) {assert.ok(!body.messages.some((m:{content?:string})=>m.content?.includes('obsolete_unique_fact')));assert.equal(body.tools.some((t:{function:{name:string}})=>t.function.name==='memory_write'),false);}
        if(rounds===2) assert.match(body.messages.at(-1).content,/obsolete_unique_fact/);
        if(rounds===3) assert.match(body.messages.at(-1).content,/current_fn/);
        const delta=rounds<3 ? {tool_calls:[{index:0,id:'read-'+rounds,type:'function',function:{name:'read_file',arguments:JSON.stringify({target_file:rounds===1?'data/memory/'+entry:'code.ts'})}}]} : {content:'Current code wins; legacy function is absent.'};
        res.writeHead(200,{'Content-Type':'text/event-stream'});res.end('data: '+JSON.stringify({choices:[{delta,finish_reason:rounds<3?'tool_calls':'stop'}]})+'\n\ndata: [DONE]\n\n');
    });
    try {
        upstream.listen(0,'127.0.0.1');await once(upstream,'listening');
        const binary=path.join(temp,'backend'),build=spawnSync('go',['build','-o',binary,'.'],{cwd:path.join(project,'backend'),encoding:'utf8'});assert.equal(build.status,0,build.stderr);
        const workspace=path.join(temp,'workspace');fs.mkdirSync(workspace);fs.writeFileSync(path.join(workspace,'code.ts'),'export function current_fn() {}');
        running=await backend(binary,temp,'deepseek-flash','v2',`http://127.0.0.1:${(upstream.address() as import('node:net').AddressInfo).port}`);
        const body='用户偏好 4 空格缩进。'+ ' durable preference'.repeat(8)+' obsolete_unique_fact: legacy_fn exists in code.ts';
        const common=['--workspace',workspace,'--server',running.url];
        const denied=await cli(['--remember',body,...common]);assert.equal(denied.code,1,denied.stderr);assert.equal(fs.existsSync(path.join(workspace,'data/memory')),false);
        const saved=await cli(['--remember',body,'--yes',...common]);assert.equal(saved.code,0,saved.stderr);assert.equal(rounds,0);
        index=fs.readFileSync(path.join(workspace,'data/memory/MEMORY.md'),'utf8');entry=/\]\(([^)]+)\)/.exec(index)![1];
        assert.match(fs.readFileSync(path.join(workspace,'data/memory',entry),'utf8'),/^---\nname:/);
        const result=await cli(['inspect memory','--mode','readonly',...common]);assert.equal(result.code,0,result.stderr);assert.match(result.stdout,/Current code wins/);assert.equal(rounds,3);
        const audit=fs.readFileSync(path.join(temp,'data/audit.jsonl'),'utf8').trim().split('\n').map(l=>JSON.parse(l));
        assert.ok(audit.some(r=>r.tool==='memory_write'&&r.status==='denied'&&r.workspaceDigest&&!r.sessionId));assert.ok(audit.some(r=>r.tool==='memory_write'&&r.status==='ok'));assert.ok(!JSON.stringify(audit).includes('obsolete_unique_fact'));
    } finally {await running?.stop();await new Promise<void>(resolve=>upstream.close(()=>resolve()));fs.rmSync(temp,{recursive:true,force:true});}
});
