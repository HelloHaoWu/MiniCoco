import {test} from 'node:test';
import assert from 'node:assert/strict';
import {BackgroundTasks} from './background.ts';
import {Conversation} from './conversation.ts';
import {RoundError} from './roundError.ts';
import {mkdtempSync,readFileSync,existsSync,rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import type {RoundArgs} from './executor.ts';

test('background queue returns immediately, caps tasks, cancels and consumes each result once',async()=>{
    const tasks=new BackgroundTasks();let started=0;
    const work=(signal:AbortSignal):Promise<string>=>new Promise((_,reject)=>{started++;signal.addEventListener('abort',()=>reject(new Error('cancelled')),{once:true});});
    for(let i=0;i<4;i++)assert(tasks.start(work));
    assert.throws(()=>tasks.start(work),/limit reached/);
    await Promise.resolve();assert.equal(started,4);assert(tasks.hasRunning);
    tasks.cancel();await tasks.wait();const results=tasks.takeCompleted();assert.equal(results.length,4);assert(results.every(r=>r.isError));assert.equal(tasks.takeCompleted().length,0);await tasks.close();
});

test('wait-background injects a same-session delta, retries it unchanged, and never reruns command',async()=>{
    const ws=mkdtempSync(join(tmpdir(),'minicoco-bg-'));let calls=0,failed=false,roundId='';
    const convo=new Conversation('http://127.0.0.1:1',ws,'',10,{approvalMode:'yes',waitBackground:true,retryPolicy:{maxRetries:1,baseMs:0,capMs:0,jitter:0},retryDependencies:{sleep:async()=>{},onRetry:()=>{},run:async(args:RoundArgs)=>{
        calls++;
        if(calls===1)return {sessionId:'s_bg',assistant:{role:'assistant'},finish:'tool_calls',toolCalls:[{id:'bg',name:'run_command',arguments:{command:'sleep 0.1; printf x >> count; echo done',run_in_background:true}}]};
        if(args.backgroundResults){
            assert.equal(args.sessionId,'s_bg');assert.match(args.backgroundResults[0].content,/done/);
            if(!failed){failed=true;roundId=args.roundId;args.backgroundResults[0].content='mutated';throw new RoundError('retry','network');}
            assert.equal(args.roundId,roundId);
        }
        return {sessionId:'s_bg',assistant:{role:'assistant',content:'complete'},finish:'stop',toolCalls:[]};
    }}});
    try{const result=await convo.run('start');assert.equal(result.status,'completed');assert.equal(calls,4);assert.equal(readFileSync(join(ws,'count'),'utf8'),'x');}
    finally{await convo.close();rmSync(ws,{recursive:true,force:true});}
});

test('conversation close terminates background descendants without late writes',async()=>{
    const ws=mkdtempSync(join(tmpdir(),'minicoco-bg-close-'));const convo=new Conversation('http://127.0.0.1:1',ws,'',25,{approvalMode:'yes'});
    try{const result=await convo.runTool({id:'bg',name:'run_command',arguments:{command:'sleep 0.2; echo bad > marker',run_in_background:true}});assert.match(result,/taskId=/);await convo.close();await new Promise(resolve=>setTimeout(resolve,250));assert(!existsSync(join(ws,'marker')));}
    finally{await convo.close();rmSync(ws,{recursive:true,force:true});}
});
