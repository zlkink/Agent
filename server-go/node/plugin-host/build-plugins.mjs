// Bundles Prometheus TypeScript plugins for the Go host's plugin bridge.
// Add a plugin name here and it becomes available to Claude/PI through the MCP
// tool bus; the bridge context lives in index.mjs.
import { build } from 'esbuild';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const repo = resolve(here, '../../..');
const alias = {
  '@prometheus/plugin-sdk': resolve(repo, 'packages/plugin-sdk/src/index.ts'),
  '@prometheus/protocol': resolve(repo, 'packages/protocol/src/index.ts'),
  '@prometheus/gateway': resolve(repo, 'packages/gateway/src/index.ts'),
  '@prometheus/subagents': resolve(repo, 'packages/subagents/src/index.ts'),
  '@prometheus/plugin-computer-use': resolve(repo, 'plugins/computer-use/src/index.ts'),
  '@prometheus/plugin-computer-use/engine/diff.ts': resolve(repo, 'plugins/computer-use/src/engine/diff.ts'),
  '@prometheus/plugin-computer-use/engine/prune.ts': resolve(repo, 'plugins/computer-use/src/engine/prune.ts'),
  '@prometheus/plugin-computer-use/engine/render.ts': resolve(repo, 'plugins/computer-use/src/engine/render.ts'),
  '@prometheus/plugin-sdk': resolve(repo, 'packages/plugin-sdk/src/index.ts'),
  typebox: resolve(here, 'node_modules/typebox'),
  'typebox/value': resolve(here, 'node_modules/typebox/build/value/index.mjs'),
  'typebox/compile': resolve(here, 'node_modules/typebox/build/compile/index.mjs'),
};

const plugins = [
  'todos', 'memory', 'folder', 'logs', 'git', 'terminal', 'ssh', 'workbench',
  'connectors', 'voice', 'subagents', 'modes', 'skills', 'automations', 'flows',
  'assistant', 'browser', 'computer-use', 'veil', 'mcp', 'packages', 'knowledge-lancedb',
];

for (const name of plugins) {
  try {
    await build({
      entryPoints: [resolve(repo, 'plugins', name, 'src/index.ts')],
      outfile: resolve(here, 'dist', `${name}.mjs`),
      bundle: true,
      platform: 'node',
      format: 'esm',
      logLevel: 'warning',
      external: ['@earendil-works/pi-coding-agent', '@earendil-works/pi-mcp', '@lancedb/lancedb', 'pg'],
      alias,
    });
    console.log(`built ${name}.mjs`);
  } catch (err) {
    console.error(`skipped ${name}: ${err?.errors?.[0]?.text || err?.message || err}`);
  }
}
