import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, existsSync, rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {createServer} from 'node:http';
import {once} from 'node:events';
import {spawn} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {Conversation} from './conversation.ts';
import type {AuditRecord} from './audit.ts';

async function fixture(run: (url: string, workspace: string, audit: AuditRecord[]) => Promise<void>): Promise<void> {
    const workspace = mkdtempSync(join(tmpdir(), 'minicoco-approval-'));
    const audit: AuditRecord[] = [];
    const server = createServer(async (req,res) => {
        let raw = '';for await(const chunk of req) raw += chunk;
        const body = JSON.parse(raw);
        if (req.url === '/audit') {audit.push(body);res.writeHead(202);res.end('{}');return;}
        res.setHeader('X-Session-Id','s_approval');res.setHeader('X-Trace-Id','s_approval-r1');res.setHeader('Content-Type','text/event-stream');
        const events = body.toolUseResults ? [{type:'text',delta:'continued'}, {type:'done',finish_reason:'stop'}] : [{type:'tool_call',id:'w',name:'write_file',arguments:{target_file:'changed',content:'secret fixture'}},{type:'done',finish_reason:'tool_calls'}];
        res.end(events.map(event=>`data: ${JSON.stringify(event)}\n\n`).join(''));
    });
    server.listen(0,'127.0.0.1');await once(server,'listening');
    try {await run(`http://127.0.0.1:${(server.address() as {port:number}).port}`,workspace,audit);}
    finally {server.closeAllConnections();await new Promise<void>(resolve=>server.close(()=>resolve()));rmSync(workspace,{recursive:true,force:true});}
}

test('approval denial continues the loop and audits digest only; readonly bypasses prompt',()=>fixture(async (url,ws,audit)=>{
    let prompts=0;
    const convo=new Conversation(url,ws,'',25,{approvalMode:'interactive',approvalPrompt:async()=>{prompts++;return false;}});
    assert.equal((await convo.run('write')).status,'completed');
    assert(!existsSync(join(ws,'changed')));assert.equal(prompts,1);
    assert.doesNotMatch(await convo.runTool({id:'read',name:'list_dir',arguments:{}}),/^ERROR/);
    assert.equal(prompts,1);await convo.close();
    assert.equal(audit.length,2);assert.equal(audit[0].approved,false);assert.equal(audit[0].status,'denied');assert.equal(audit[1].status,'ok');assert.match(audit[0].argsDigest,/^[a-f0-9]{12}$/);assert(!JSON.stringify(audit).includes('secret fixture'));
}));

test('non-TTY CLI denies writes by default; --yes explicitly approves and audits',()=>fixture(async(url,ws,audit)=>{
    const entry=fileURLToPath(new URL('./cli.ts',import.meta.url));
    for(const yes of [false,true]){
        const child=spawn(process.execPath,[entry,'--workspace',ws,'--server',url,...(yes?['--yes']:[])],{stdio:['pipe','pipe','pipe']});
        let output='',errors='';child.stdout.on('data',chunk=>output+=chunk);child.stderr.on('data',chunk=>errors+=chunk);child.stdin.end('write\n');
        assert.equal((await once(child,'close'))[0],0,errors);assert.match(output,/continued/);assert.equal(existsSync(join(ws,'changed')),yes);
        if(!yes)assert.match(output,/denied by user/);
    }
    assert.deepEqual(audit.map(a=>[a.approved,a.status]),[[false,'denied'],[true,'ok']]);
}));

test('cancelled approval never executes a tool',()=>fixture(async(url,ws)=>{
    const controller=new AbortController();
    const convo=new Conversation(url,ws,'',25,{approvalMode:'interactive',approvalPrompt:async()=>{controller.abort();controller.signal.throwIfAborted();return true;}});
    const result=await convo.run('write',controller.signal);await convo.close();
    assert.equal(result.status,'cancelled');assert(!existsSync(join(ws,'changed')));
}));
