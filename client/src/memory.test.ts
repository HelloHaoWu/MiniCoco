import {test} from 'node:test';
import assert from 'node:assert/strict';
import * as fs from 'node:fs';
import * as os from 'node:os';
import * as path from 'node:path';
import {MemoryWrite, readMemoryIndex, rememberArgs} from './memory.ts';
import {HandlerContext} from './tools/base.ts';
import {Conversation} from './conversation.ts';
import {RoundError} from './roundError.ts';

const args={name:'缩进偏好',description:'用户偏好 4 空格缩进',type:'preference',body:'durable body only'};
test('memory entry/index update/delete, workspace isolation, bounds and symlink guards',()=>{
    const dir=fs.mkdtempSync(path.join(os.tmpdir(),'minicoco-memory-'));
    try {
        fs.mkdirSync(path.join(dir,'a'));fs.mkdirSync(path.join(dir,'b'));
        const a=new HandlerContext(path.join(dir,'a')),b=new HandlerContext(path.join(dir,'b')),writer=new MemoryWrite(a);
        assert.equal(readMemoryIndex(a),'');writer.execute(args);
        const index=readMemoryIndex(a),filename=/\]\(([^)]+)\)/.exec(index)![1];
        assert.ok(index.includes(args.description));assert.ok(!index.includes(args.body));assert.equal(readMemoryIndex(b),'');
        const body=fs.readFileSync(a.resolve('data/memory/'+filename),'utf8');assert.match(body,/^---\nname: "缩进偏好"\ndescription: "用户偏好 4 空格缩进"\ntype: preference\n---\n/);
        writer.execute({...args,body:'updated',description:'new hook'});assert.equal(readMemoryIndex(a).trim().split('\n').length,1);assert.match(fs.readFileSync(a.resolve('data/memory/'+filename),'utf8'),/updated/);
        writer.execute({...args,name:'long'.repeat(30),description:'😊'.repeat(250)});assert.ok(readMemoryIndex(a).split('\n').every(l=>[...l].length<=150));
        assert.throws(()=>writer.execute({...args,type:'secret'}));assert.throws(()=>writer.execute({...args,body:'x'.repeat(65537)}));
        writer.execute({name:args.name,operation:'delete'});assert.equal(fs.existsSync(a.resolve('data/memory/'+filename)),false);assert.ok(!readMemoryIndex(a).includes(filename));
        fs.symlinkSync(a.resolve('data'),b.resolve('data'));assert.throws(()=>new MemoryWrite(b).execute(args),/symlink/);
        fs.writeFileSync(a.resolve('data/memory/MEMORY.md'),'x'.repeat(151));assert.throws(()=>readMemoryIndex(a),/150 characters/);
        assert.equal(rememberArgs('偏好 4 空格').type,'preference');
    } finally {fs.rmSync(dir,{recursive:true,force:true});}
});
test('memory approval, readonly denial, refreshed next round and immutable retry snapshot',async()=>{
    const dir=fs.mkdtempSync(path.join(os.tmpdir(),'minicoco-memory-round-'));
    const denied=new Conversation('http://127.0.0.1:1',dir,'mock',5,{quiet:true,approvalMode:'deny'});
    const readonly=new Conversation('http://127.0.0.1:1',dir,'mock',5,{quiet:true,approvalMode:'yes',mode:'readonly'});
    let count=0,roundId='';
    const convo=new Conversation('http://127.0.0.1:1',dir,'mock',5,{quiet:true,approvalMode:'yes',retryDependencies:{sleep:async()=>{},onRetry:()=>{},run:async request=>{
        count++;
        if(count===1){assert.equal(request.memoryIndex,undefined);return {sessionId:'memory-session',assistant:{role:'assistant'},toolCalls:[{id:'save',name:'memory_write',arguments:args}],finish:'tool_calls'};}
        assert.ok(request.memoryIndex?.includes(args.description));assert.ok(!request.memoryIndex?.includes(args.body));
        if(count===2){roundId=request.roundId;fs.writeFileSync(path.join(dir,'data/memory/MEMORY.md'),'changed on disk\n');request.memoryIndex='mutated transport';throw new RoundError('retry','network');}
        assert.equal(request.roundId,roundId);assert.match(request.toolUseResults![0].output,/Saved memory/);
        return {sessionId:'memory-session',assistant:{role:'assistant',content:'done'},toolCalls:[],finish:'stop'};
    }}});
    try {
        assert.match(await denied.runTool({id:'d',name:'memory_write',arguments:args}),/denied/);
        assert.match(await readonly.runTool({id:'d',name:'memory_write',arguments:args}),/not allowed/);
        assert.equal(fs.existsSync(path.join(dir,'data')),false);
        assert.equal((await convo.run('remember')).status,'completed');assert.equal(count,3);
        assert.equal(readMemoryIndex(convo.ctx),'changed on disk\n');
        fs.writeFileSync(path.join(dir,'data/memory/MEMORY.md'),'x'.repeat(151));
        const writer=new Conversation('http://127.0.0.1:1',dir,'mock',3,{quiet:true,approvalMode:'yes',retryDependencies:{run:async request=>({sessionId:'invalid-index',assistant:{role:'assistant',content:request.memoryIndex??'omitted'},toolCalls:[],finish:'stop'})}});
        try {assert.equal((await writer.run('inspect')).text,'omitted');} finally {await writer.close();}
    } finally {await denied.close();await readonly.close();await convo.close();fs.rmSync(dir,{recursive:true,force:true});}
});
