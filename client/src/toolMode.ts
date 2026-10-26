import type {ToolMode} from './protocol.ts';

const allTools = new Set(['read_file', 'list_dir', 'grep', 'write_file', 'edit_file', 'run_command', 'delegate_subagent']);
const writesOrExecutes = new Set(['write_file', 'edit_file', 'run_command', 'delegate_subagent']);

export function normalizeMode(value: unknown): ToolMode {
    if (value === undefined || value === '' || value === 'full') return 'full';
    if (value === 'readonly' || value === 'safe') return value;
    throw new Error('mode must be full, readonly or safe');
}

export function toolAllowed(mode: ToolMode, name: string): boolean {
    return allTools.has(name) && !(mode === 'readonly' && writesOrExecutes.has(name)) && !(mode === 'safe' && name === 'run_command');
}
