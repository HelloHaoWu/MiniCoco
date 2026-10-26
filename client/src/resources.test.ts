import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, readFileSync, rmSync, existsSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {createHandler, HandlerContext} from './tools/index.ts';

async function fixture(env: Record<string,string>, run:(ctx:HandlerContext)=>Promise<void>):Promise<void>{
    const ws=mkdtempSync(join(tmpdir(),'minicoco-resources-'));const old={...process.env};Object.assign(process.env,env);
    try{await run(new HandlerContext(ws));}finally{for(const key of Object.keys(env)){if(old[key]===undefined)delete process.env[key];else process.env[key]=old[key];}rmSync(ws,{recursive:true,force:true});}
}

test('RSS guard kills the shell process group and its allocating descendant',{skip:process.platform==='win32'},()=>fixture({RSS_CAP:String(60*1024*1024)},async ctx=>{
    const command=`"${process.execPath}" -e "require('node:fs').writeFileSync('pid',String(process.pid));const b=Buffer.alloc(100*1024*1024,1);setInterval(()=>b[0],1000)"`;
    await assert.rejects(async()=>createHandler('run_command',ctx)!.execute({command}),/killed \(memory\)/);
    const pid=Number(readFileSync(join(ctx.workspace,'pid'),'utf8'));
    let live=true;for(let i=0;i<50&&live;i++){try{process.kill(pid,0);}catch(error){if((error as NodeJS.ErrnoException).code==='ESRCH')live=false;else throw error;}if(live)await new Promise(resolve=>setTimeout(resolve,10));}
    assert.equal(live,false,'allocating descendant survived');
}));

test('wall clock limit terminates command descendants',()=>fixture({RUN_TIMEOUT_MS:'50'},async ctx=>{
    await assert.rejects(async()=>createHandler('run_command',ctx)!.execute({command:'(sleep 0.2; echo survived > marker) & wait'}),/timed out/);
    await new Promise(resolve=>setTimeout(resolve,250));assert(!existsSync(join(ctx.workspace,'marker')));
}));

test('invalid resource configuration prevents command execution',()=>fixture({RUN_OUTPUT_CAP:'0'},async ctx=>{
    await assert.rejects(async()=>createHandler('run_command',ctx)!.execute({command:'echo bad > marker'}),/RUN_OUTPUT_CAP/);
    assert(!existsSync(join(ctx.workspace,'marker')));
}));
