import type {ToolMode} from './protocol.ts';
export type SceneName = 'composer' | 'qa' | 'review';
export function sceneFor(value?: string): SceneName {return value === 'qa' || value === 'review' ? value : 'composer';}
export function sceneMode(mode: ToolMode, scene: SceneName): ToolMode {return scene === 'composer' ? mode : 'readonly';}
export function parseSceneQuery(query: string, fallback?: string): {scene: SceneName; query: string} {
    const match = query.match(/^\s*\/(qa|review|composer)(?:\s+|$)/);
    const text = match ? query.slice(match[0].length).trim() : query;
    if (!text.trim()) throw new Error('scene command requires a query');
    return {scene: sceneFor(match?.[1] ?? fallback), query: text};
}
