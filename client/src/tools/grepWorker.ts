import {parentPort, workerData} from 'node:worker_threads';
import {HandlerContext} from './base.ts';
import {search} from './grep.ts';

try {
    parentPort!.postMessage({result: search(new HandlerContext(workerData.workspace), workerData.args)});
} catch (error) {
    parentPort!.postMessage({error: (error as Error).message});
} finally { parentPort!.close(); }
