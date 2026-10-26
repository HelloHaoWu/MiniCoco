import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync,existsSync,rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {Conversation} from './conversation.ts';
import {parseSceneQuery} from './scenes.ts';

test('slash query and options select scenes; qa/review cannot execute writes even with --yes',async()=>{
    const ws=mkdtempSync(join(tmpdir(),'minicoco-scene-'));
    try{
        for(const scene of ['qa','review']){
            const convo=new Conversation('http://127.0.0.1:1',ws,'',25,{approvalMode:'yes',retryDependencies:{run:async args=>{
                assert.equal(args.slash,scene);assert.equal(args.mode,'readonly');assert.equal(args.query,'inspect');
                return {sessionId:'s_scene',assistant:{role:'assistant'},finish:'tool_calls',toolCalls:[{id:'bad',name:'write_file',arguments:{target_file:'bad',content:'x'}}]};
            }}});
            assert.equal((await convo.run('/'+scene+' inspect')).status,'failed');assert(!existsSync(join(ws,'bad')));await convo.close();
        }
        const convo=new Conversation('http://127.0.0.1:1',ws,'',25,{mode:'safe',slash:'qa',retryDependencies:{run:async args=>{assert.equal(args.mode,'safe');assert.equal(args.slash,undefined);return {sessionId:'s_scene',assistant:{role:'assistant'},finish:'stop',toolCalls:[]};}}});
        assert.equal((await convo.run('/composer inspect')).status,'completed');await convo.close();
        assert.deepEqual(parseSceneQuery('inspect','unknown'),{scene:'composer',query:'inspect'});assert.throws(()=>parseSceneQuery('/qa'),/requires a query/);
    }finally{rmSync(ws,{recursive:true,force:true});}
});
