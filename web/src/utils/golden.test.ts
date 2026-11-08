// Cross-checks logic that exists in both the web client and the Go server
// against shared fixtures in testdata/golden. Run with UPDATE_GOLDEN=1 to
// regenerate the expected file from this (reference) implementation.
import { createHash } from 'node:crypto';
import { readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';

import { describe, expect, it } from 'vitest';

import type { CommentThread, DiffCommentThread } from '../types/diff';

import { formatCommentsOutput } from './commentFormatting';
import { parseCommentImportValue, serializeCommentImports } from './commentImports';
import { buildEditorSpawnSpec, parseEditorArgsTemplate } from './editorOptions';

interface Fixtures {
  output: Array<{ name: string; threads: DiffCommentThread[] }>;
  imports: Array<{ name: string; input?: unknown; raw?: string }>;
  editorArgs: Array<{ template: string }>;
}

const goldenDir = resolve(__dirname, '../../../testdata/golden');
const fixtures = JSON.parse(readFileSync(resolve(goldenDir, 'comments.json'), 'utf8')) as Fixtures;
const expectedPath = resolve(goldenDir, 'comments.expected.json');

function toCommentThread(thread: DiffCommentThread): CommentThread {
  return {
    id: thread.id,
    file: thread.filePath,
    line:
      typeof thread.position.line === 'number'
        ? thread.position.line
        : [thread.position.line.start, thread.position.line.end],
    side: thread.position.side,
    createdAt: thread.createdAt,
    updatedAt: thread.updatedAt,
    codeContent: thread.codeSnapshot?.content,
    messages: thread.messages,
  };
}

function computeImport(raw: string): Record<string, unknown> {
  try {
    const imports = parseCommentImportValue(raw);
    const serialized = serializeCommentImports(imports);
    return {
      serialized,
      importId: createHash('sha256').update(serialized).digest('hex'),
    };
  } catch (error) {
    return { error: error instanceof Error ? error.message : String(error) };
  }
}

function computeAll() {
  return {
    output: fixtures.output.map((c) => ({
      name: c.name,
      text: formatCommentsOutput(c.threads.map(toCommentThread)),
    })),
    imports: fixtures.imports.map((c) => ({
      name: c.name,
      ...computeImport(c.raw ?? JSON.stringify(c.input)),
    })),
    editorArgs: fixtures.editorArgs.map((c) => ({
      template: c.template,
      args: parseEditorArgsTemplate(c.template),
      spawnArgs: buildEditorSpawnSpec({
        command: 'editor',
        argsTemplate: c.template,
        filePath: '/repo/dir with space/file.ts',
        lineNumber: 42,
      })?.args,
    })),
  };
}

describe('golden fixtures shared with the Go server', () => {
  it('matches testdata/golden/comments.expected.json', () => {
    const actual = computeAll();
    if (process.env.UPDATE_GOLDEN) {
      writeFileSync(expectedPath, `${JSON.stringify(actual, null, 2)}\n`);
    }
    const expected: unknown = JSON.parse(readFileSync(expectedPath, 'utf8'));
    expect(actual).toEqual(expected);
  });
});
