import {test} from 'node:test';
import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import {StdioMcp, MCPManager} from './mcp.ts';
import {Conversation} from './conversation.ts';
import {consumeEvents} from './executor.ts';
import {toolAllowed} from './toolMode.ts';

function fixture(): {dir:string;script:string;config:string} {
    const dir=fs.mkdtempSync(path.join(os.tmpdir(),'minicoco-mcp-')),script=path.join(dir,'server.cjs'),config=path.join(dir,'mcp.json');
    fs.writeFileSync(script,`const rl=require('node:readline').createInterface({input:process.stdin});let initialized=false;const out=(id,result)=>process.stdout.write(JSON.stringify({jsonrpc:'2.0',id,result})+'\\n');rl.on('line',line=>{const m=JSON.parse(line);if(m.method==='initialize')out(m.id,{protocolVersion:'2024-11-05',capabilities:{tools:{}}});else if(m.method==='notifications/initialized')initialized=true;else if(m.method==='tools/list'){if(!initialized)process.exit(2);out(m.id,{tools:[{name:'ping',description:'echo',inputSchema:{type:'object',properties:{msg:{type:'string'}}}}]});}else if(m.method==='tools/call'){if(m.params.arguments.exit)process.exit(0);if(m.params.arguments.wait)return;setTimeout(()=>out(m.id,{content:[{type:'text',text:m.params.arguments.msg||'pong'}],isError:!!m.params.arguments.fail}),m.params.arguments.delay||0);}else if(m.method==='notifications/cancelled')require('node:fs').appendFileSync('cancelled',String(m.params.requestId)+'\\n');});`);
    fs.writeFileSync(config,JSON.stringify({servers:[{name:'missing',transport:'stdio',command:path.join(dir,'missing')},{name:'echo',transport:'stdio',command:process.execPath,args:[script],readOnly:true},{name:'writer',transport:'stdio',command:process.execPath,args:[script]}]}));
    return {dir,script,config};
}
test('MCP handshake/discovery, isolation, readonly policy, approval and result continuation',async()=>{
    const f=fixture();const manager=await MCPManager.connect(f.config,f.dir);
    const denied=new Conversation('http://127.0.0.1:1',f.dir,'mock',4,{mcp:manager,quiet:true,approvalMode:'deny'});
    try {
        const catalog=manager.catalog();assert.deepEqual(catalog.map(t=>t.name),['mcp__echo__ping','mcp__writer__ping']);
        assert.equal(toolAllowed('readonly',catalog[0].name,catalog),true);assert.equal(toolAllowed('safe',catalog[1].name,catalog),false);assert.equal(toolAllowed('full','mcp__unknown__ping',catalog),false);
        assert.match(await denied.runTool({id:'w',name:catalog[1].name,arguments:{msg:'write'}}),/denied by user/);
        assert.equal(await denied.runTool({id:'r',name:catalog[0].name,arguments:{msg:'hello'}}),'hello');
        const calls=[{type:'tool_call',id:'1',name:catalog[0].name,arguments:{msg:'echo'}},{type:'done',finish_reason:'tool_calls'}];
        assert.equal((await consumeEvents(calls,'readonly',catalog)).toolCalls[0].name,catalog[0].name);
        await assert.rejects(()=>consumeEvents([{...calls[0],name:catalog[1].name},calls[1]],'readonly',catalog));
        let round=0;
        const convo=new Conversation('http://127.0.0.1:1',f.dir,'mock',4,{mcp:manager,quiet:true,approvalMode:'yes',retryDependencies:{run:async args=>{
            assert.equal(args.mcpTools?.length,2);round++;
            if(round===1)return {sessionId:'mcp-session',assistant:{role:'assistant'},toolCalls:[{id:'c',name:catalog[1].name,arguments:{msg:'written'}}],finish:'tool_calls'};
            assert.equal(args.toolUseResults?.[0].output,'written');return {sessionId:'mcp-session',assistant:{role:'assistant',content:'done'},toolCalls:[],finish:'stop'};
        }}});
        try {assert.equal((await convo.run('echo')).status,'completed');} finally {await convo.close();}
        assert.match(await denied.runTool({id:'r',name:catalog[0].name,arguments:{}}),/unavailable/);
    } finally {await denied.close();fs.rmSync(f.dir,{recursive:true,force:true});}
});
test('MCP concurrent IDs, cancellation, timeout, tool errors and process exit are bounded',async()=>{
    const f=fixture(), rpc=new StdioMcp(process.execPath,[f.script],f.dir,150);
    try {
        const results=await Promise.all([rpc.rpc('tools/call',{arguments:{msg:'slow',delay:40}}),rpc.rpc('tools/call',{arguments:{msg:'fast'}})]);
        assert.equal((results[0] as {content:{text:string}[]}).content[0].text,'slow');
        const controller=new AbortController();const pending=rpc.rpc('tools/call',{arguments:{wait:true}},controller.signal);controller.abort();await assert.rejects(pending,/cancelled/);
        await assert.rejects(rpc.rpc('tools/call',{arguments:{wait:true}}),/timeout/);
        assert.ok(fs.readFileSync(path.join(f.dir,'cancelled'),'utf8').length);
        await assert.rejects(rpc.rpc('tools/call',{arguments:{exit:true}}),/unavailable/);
    } finally {await rpc.close();fs.rmSync(f.dir,{recursive:true,force:true});}
});
