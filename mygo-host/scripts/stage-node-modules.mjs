#!/usr/bin/env node
// Stage the node_modules closure needed by the packaged db-worker-node
// sidecar into <resources>/node_modules. The release sidecar bundles all
// CLJS code but still require()s npm packages at runtime (js-provider
// :require); inside a package there is no repo node_modules to resolve
// from, so every external module must ship next to the bundle.
//
// Usage: node stage-node-modules.mjs <path-to-db-worker-node.js> <resources-dir>
//
// The require list is extracted from the sidecar itself, so newly added
// runtime deps are picked up automatically. @zvec platform bindings use a
// dynamic require('@zvec/bindings-<platform>-<arch>') and are staged for
// the current platform only.

import fs from 'node:fs';
import path from 'node:path';
import mod from 'node:module';
import { execFileSync } from 'node:child_process';

const [, , sidecarPath, resourcesDir] = process.argv;
if (!sidecarPath || !resourcesDir) {
  console.error('usage: stage-node-modules.mjs <db-worker-node.js> <resources-dir>');
  process.exit(1);
}

const repoRoot = path.resolve(path.dirname(sidecarPath), '..');
const outDir = path.join(resourcesDir, 'node_modules');
const requireFromRepo = mod.createRequire(path.join(repoRoot, 'package.json'));

const source = fs.readFileSync(sidecarPath, 'utf8');
const names = new Set();
for (const m of source.matchAll(/require\((["'])([^"')]+)\1\)/g)) {
  const name = m[2];
  if (name.startsWith('.') || name.startsWith('/') || mod.isBuiltin(name) || name.startsWith('node:')) continue;
  names.add(name);
}
// Dynamic native bindings for the current platform (@zvec/zvec resolves
// bindings-<platform>-<arch> at runtime, invisible to static scanning).
names.add(`@zvec/bindings-${process.platform}-${process.arch}`);

function packageDir(name, fromDir) {
  let resolved;
  try {
    resolved = requireFromRepo.resolve(name, { paths: [fromDir, repoRoot] });
  } catch {
    return null;
  }
  let dir = path.dirname(resolved);
  const want = name.split('/');
  for (let i = 0; i < 12 && dir !== path.parse(dir).root; i++) {
    const base = path.basename(dir);
    const scope = path.basename(path.dirname(dir));
    if (base === want[want.length - 1] && (want.length === 1 || scope === want[0])) {
      return fs.realpathSync(dir);
    }
    dir = path.dirname(dir);
  }
  return null;
}

const staged = new Map(); // name -> dir (realpath)
const queue = [...names].map((name) => ({ name, fromDir: repoRoot }));
while (queue.length) {
  const { name, fromDir } = queue.shift();
  if (staged.has(name)) continue;
  const dir = packageDir(name, fromDir);
  if (!dir) {
    console.error(`[stage-node-modules] warning: cannot resolve ${name}`);
    continue;
  }
  staged.set(name, dir);
  const pkg = JSON.parse(fs.readFileSync(path.join(dir, 'package.json'), 'utf8'));
  for (const key of ['dependencies', 'optionalDependencies']) {
    for (const dep of Object.keys(pkg[key] || {})) {
      if (!mod.isBuiltin(dep)) queue.push({ name: dep, fromDir: dir });
    }
  }
}

fs.rmSync(outDir, { recursive: true, force: true });
fs.mkdirSync(outDir, { recursive: true });
let bytes = 0;
for (const [name, dir] of [...staged.entries()].sort(([a], [b]) => a.localeCompare(b))) {
  const dest = path.join(outDir, name);
  fs.mkdirSync(path.dirname(dest), { recursive: true });
  fs.cpSync(dir, dest, { recursive: true, dereference: true });
  bytes += Number(execFileSync('du', ['-sk', dest], { encoding: 'utf8' }).split('\t')[0]) * 1024;
}
console.log(`[stage-node-modules] staged ${staged.size} packages (${(bytes / 1048576).toFixed(1)}MB) -> ${outDir}`);
