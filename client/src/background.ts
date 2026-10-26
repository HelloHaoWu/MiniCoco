import {randomUUID} from 'node:crypto';
import {boundedOutput, ToolError} from './tools/base.ts';

export interface BackgroundResult {taskId: string; content: string; isError: boolean;}
interface Task {controller: AbortController; done: Promise<void>; result?: BackgroundResult;}

export class BackgroundTasks {
    private tasks = new Map<string, Task>();
    start(work: (signal: AbortSignal) => Promise<string>, signal?: AbortSignal): string {
        if (this.tasks.size >= 4) throw new ToolError('background task limit reached (4 unconsumed tasks)');
        signal?.throwIfAborted();
        const taskId = randomUUID(), controller = new AbortController();
        const task: Task = {controller, done: Promise.resolve()};
        const combined = signal ? AbortSignal.any([signal, controller.signal]) : controller.signal;
        task.done = Promise.resolve().then(() => work(combined)).then(
            content => {task.result = {taskId, content: boundedOutput(content), isError: content.startsWith('ERROR:')};},
            error => {task.result = {taskId, content: boundedOutput(`ERROR: ${(error as Error).message}`), isError: true};},
        );
        this.tasks.set(taskId, task);return taskId;
    }
    get hasCompleted(): boolean {return [...this.tasks.values()].some(t => !!t.result);}
    get hasRunning(): boolean {return [...this.tasks.values()].some(t => !t.result);}
    takeCompleted(): BackgroundResult[] {
        const results: BackgroundResult[] = [];
        for (const [id, task] of this.tasks) if (task.result) {results.push(task.result);this.tasks.delete(id);}
        return results;
    }
    async wait(signal?: AbortSignal): Promise<void> {
        const abort = (): void => this.cancel();
        signal?.addEventListener('abort', abort, {once: true});if (signal?.aborted) abort();
        try {await Promise.all([...this.tasks.values()].map(t => t.done));signal?.throwIfAborted();}
        finally {signal?.removeEventListener('abort', abort);}
    }
    cancel(): void {for (const task of this.tasks.values()) task.controller.abort(new Error('background tasks cancelled'));}
    async close(): Promise<void> {this.cancel();await this.wait();this.tasks.clear();}
}
