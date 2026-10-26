import * as fs from 'node:fs';
import {createHash} from 'node:crypto';
import {Handler, HandlerContext, ToolError, stringArg, type ToolArgs} from './tools/base.ts';

export const MEMORY_INDEX_BYTES = 16 * 1024;
const indexPath = 'data/memory/MEMORY.md';
export function readMemoryIndex(ctx: HandlerContext): string {
    let text: string;
    try {text=ctx.readText(ctx.resolve(indexPath));} catch(error) {if ((error as NodeJS.ErrnoException).code==='ENOENT') return '';throw error;}
    if (Buffer.byteLength(text)>MEMORY_INDEX_BYTES || text.includes('\0') || text.split('\n').some(line=>[...line].length>150)) throw new ToolError('memory index exceeds 16 KiB or 150 characters per line');
    return text;
}
function singleLine(value: string): string {return value.replace(/[\r\n\t]/g,' ').trim();}
function title(value: string): string {return singleLine(value).replace(/[\[\]\\]/g,'');}
export function rememberArgs(body: string): ToolArgs {
    const text=singleLine(body);
    if(!text)throw new ToolError('memory body must not be empty');
    return {name:'remember-'+createHash('sha256').update(body).digest('hex').slice(0,12),description:[...text].slice(0,80).join(''),type:'preference',body};
}
export class MemoryWrite extends Handler {
    name='memory_write';
    requiresApproval=true;
    protected run(args: ToolArgs): string {
        const name=stringArg(args,'name');
        if(name.length>128 || name!==singleLine(name))throw new ToolError('memory name must be one line, at most 128 characters');
        const operation=args.operation ?? 'write';
        if(operation!=='write'&&operation!=='delete')throw new ToolError('operation must be write or delete');
        // Stable opaque filenames keep human titles out of paths and make updates idempotent.
        const filename='mem_'+createHash('sha256').update(name).digest('hex').slice(0,24)+'.md';
        const entry=this.ctx.resolve('data/memory/'+filename),index=this.ctx.resolve(indexPath);
        let lines=readMemoryIndex(this.ctx).split('\n').filter(line=>line && !line.includes(']('+filename+')'));
        if(operation==='delete') {
            this.ctx.writeText(index,lines.length?lines.join('\n')+'\n':'');
            this.ctx.resolve(entry);fs.rmSync(entry,{force:true});
            return `Deleted memory ${filename}`;
        }
        const description=stringArg(args,'description'),body=stringArg(args,'body'),type=stringArg(args,'type');
        if(description!==singleLine(description)||description.length>512||!['user','project','preference'].includes(type))throw new ToolError('invalid memory description or type');
        if(Buffer.byteLength(body)>64*1024)throw new ToolError('memory body exceeds 64 KiB');
        const label=[...title(name)].slice(0,40).join('')||'Memory';
        const prefix=`- [${label}](${filename}) — `;
        const line=prefix+[...singleLine(description)].slice(0,150-[...prefix].length).join('');
        lines.push(line);const text=lines.join('\n')+'\n';
        if(Buffer.byteLength(text)>MEMORY_INDEX_BYTES)throw new ToolError('memory index exceeds 16 KiB');
        // Publish the entry first, then its index. Failure can leave an unindexed entry,
        // but never a new index pointing to an entry that was not written.
        const frontmatter=`---\nname: ${JSON.stringify(name)}\ndescription: ${JSON.stringify(description)}\ntype: ${type}\n---\n${body}\n`;
        this.ctx.writeText(entry,frontmatter);fs.chmodSync(entry,0o600);
        this.ctx.writeText(index,text);fs.chmodSync(index,0o600);
        return `Saved memory data/memory/${filename}`;
    }
}
