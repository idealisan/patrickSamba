#!/usr/bin/env node
// Replica of CodeBuddy's CommandUtils.checkCommandSafety risk classifier.
// The regex tables and helper functions are extracted verbatim at runtime from
// the installed bundle, so this harness cannot drift from the real implementation.
//
// Usage: node risk-replica.js "<command>" ["<command>" ...]
//        node risk-replica.js --file <path-with-one-command-per-line>

const fs = require('fs');

const BUNDLE = process.env.CODEBUDDY_BUNDLE
  || '/usr/local/lib/node_modules/@tencent-ai/codebuddy-code/dist/codebuddy.js';

const src = fs.readFileSync(BUNDLE, 'utf8');

function extract(startMarker, endMarker) {
  const i = src.indexOf(startMarker);
  if (i < 0) throw new Error('marker not found: ' + startMarker);
  const j = src.indexOf(endMarker, i);
  if (j < 0) throw new Error('end marker not found: ' + endMarker);
  return src.slice(i, j);
}

// Helper functions, the RiskLevel enum IIFE and the five regex tables all live in
// one contiguous region of the bundle. `eu` is the RiskLevel enum object declared
// further up in the module, so we re-declare it inside the sandbox.
const region = extract('function extractCommandName(', 'let CommandUtils=');
const tables = eval('(function(){var eu;' + region + 'return {eC,ey,ew,eB,e_,'
  + 'extractCommandName,stripRedirections,splitCommandRespectingQuotes,'
  + 'normalizeCommandPaths,stripHeredocBodies};})()');

const { eC: KNOWN, ey: SAFE, ew: CRITICAL, eB: HIGH, e_: MEDIUM,
        extractCommandName, stripRedirections, splitCommandRespectingQuotes,
        normalizeCommandPaths, stripHeredocBodies } = tables;

const PRIORITY = { safe: 0, low: 1, medium: 2, high: 3, critical: 4 };

function checkSingle(cmd) {
  for (const re of SAFE) if (re.test(cmd)) return { riskLevel: 'safe', rule: String(re), table: 'ey/SAFE' };
  for (const re of CRITICAL) if (re.test(cmd)) return { riskLevel: 'critical', rule: String(re), table: 'ew/CRITICAL' };
  for (const re of HIGH) if (re.test(cmd)) return { riskLevel: 'high', rule: String(re), table: 'eB/HIGH' };
  for (const re of MEDIUM) if (re.test(cmd)) return { riskLevel: 'medium', rule: String(re), table: 'e_/MEDIUM' };
  const name = extractCommandName(cmd.trim());
  if (KNOWN.some((k) => k.toLowerCase() === name.toLowerCase())) {
    return { riskLevel: 'safe', rule: `known command "${name}"`, table: 'eC/KNOWN' };
  }
  return { riskLevel: 'low', rule: `unknown command "${name}"`, table: 'unknown' };
}

function crossSeparator(s) {
  if (/\|\s*xargs\s+(rm|unlink)\b/.test(s)) return { riskLevel: 'high', rule: 'cross: |xargs rm', table: 'cross' };
  if (/\b(curl|wget)\b.*\|\s*(bash|sh|zsh|ksh)\b/.test(s)) return { riskLevel: 'medium', rule: 'cross: curl|sh', table: 'cross' };
  return undefined;
}

function classify(raw) {
  const lowered = normalizeCommandPaths(stripHeredocBodies(raw)).toLowerCase();
  const stripped = stripRedirections(lowered);
  const parts = splitCommandRespectingQuotes(stripped);
  const detail = [];
  let worst = { riskLevel: 'safe', rule: '(no rule matched)', table: '-' };
  if (parts.length > 1) {
    const cross = crossSeparator(stripped);
    if (cross) return { verdict: cross, parts, detail, note: 'cross-separator rule short-circuits' };
    for (const p of parts) {
      const r = checkSingle(p);
      detail.push({ segment: p, ...r });
      if (PRIORITY[r.riskLevel] > PRIORITY[worst.riskLevel]) worst = r;
    }
  } else {
    // NOTE: single-command path feeds the NON-redirection-stripped string.
    const r = checkSingle(lowered);
    detail.push({ segment: lowered, ...r });
    worst = r;
  }
  return { verdict: worst, parts, detail };
}

let cmds = process.argv.slice(2);
if (cmds[0] === '--file') {
  cmds = fs.readFileSync(cmds[1], 'utf8').split('\n').filter((l) => l.trim() && !l.startsWith('#'));
}

for (const c of cmds) {
  const r = classify(c);
  console.log('CMD: ' + c.replace(/\n/g, '\\n'));
  console.log('  => ' + r.verdict.riskLevel.toUpperCase()
    + '   [' + r.verdict.table + '] ' + r.verdict.rule);
  console.log('  segments: ' + r.parts.length);
  for (const d of r.detail) {
    if (d.riskLevel === 'safe' || d.riskLevel === 'low') continue;
    console.log('     * ' + d.riskLevel.toUpperCase() + ' <- ' + d.segment.slice(0, 110));
    console.log('       rule ' + d.rule);
  }
  console.log('');
}
