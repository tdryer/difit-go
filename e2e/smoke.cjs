// End-to-end smoke tests: serve real repositories with the difit binary,
// drive the UI in Chromium, and check watcher refreshes, comments added from
// the UI and the CLI, the untracked-files prompt, background mode, and the
// shutdown-on-tab-close behavior.
//
// Usage: node e2e/smoke.cjs [path/to/difit] [screenshot-dir]
// Needs the playwright package (NODE_PATH may point at a global install).
// E2E_ONLY=<test name> runs a single test.

const { spawn, execFileSync } = require('node:child_process');
const fs = require('node:fs');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');
const { chromium } = require('playwright');

const binary = path.resolve(process.argv[2] ?? 'bin/difit');
const screenshotDir = process.argv[3];

const cleanups = [];

function git(cwd, ...args) {
  return execFileSync('git', args, { cwd, encoding: 'utf8' }).trim();
}

function tempDir(prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  cleanups.push(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function write(repo, file, content) {
  fs.mkdirSync(path.dirname(path.join(repo, file)), { recursive: true });
  fs.writeFileSync(path.join(repo, file), content);
}

function createRepo() {
  const dir = tempDir('difit-e2e-');
  git(dir, 'init', '-q', '-b', 'main');
  git(dir, 'config', 'user.name', 'E2E');
  git(dir, 'config', 'user.email', 'e2e@example.com');
  git(dir, 'config', 'commit.gpgsign', 'false');
  write(dir, 'src/greeting.ts', 'export const greeting = "Hello";\n');
  git(dir, 'add', '-A');
  git(dir, 'commit', '-q', '-m', 'initial');
  write(dir, 'src/greeting.ts', 'export const greeting = "Hello, difit";\n');
  git(dir, 'commit', '-q', '-am', 'greet difit');
  return dir;
}

function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      server.close(() => resolve(port));
    });
    server.on('error', reject);
  });
}

function waitFor(predicate, message, timeoutMs = 15000) {
  return new Promise((resolve, reject) => {
    const started = Date.now();
    const timer = setInterval(() => {
      if (predicate()) {
        clearInterval(timer);
        resolve();
      } else if (Date.now() - started > timeoutMs) {
        clearInterval(timer);
        reject(new Error(`Timed out waiting for ${message}`));
      }
    }, 50);
  });
}

function isRunning(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

// startDifit runs difit in the foreground and collects its output.
async function startDifit(repo, args, { stdin } = {}) {
  const port = await freePort();
  const env = { ...process.env, DIFIT_CONFIG_DIR: tempDir('difit-e2e-config-') };
  const proc = { port, output: '', exitCode: null };
  const child = spawn(binary, [...args, '--no-open', '--port', String(port)], {
    cwd: repo,
    env,
    stdio: [stdin === undefined ? 'ignore' : 'pipe', 'pipe', 'pipe'],
  });
  cleanups.push(() => proc.exitCode === null && child.kill());
  child.stdout.on('data', (chunk) => (proc.output += chunk));
  child.stderr.on('data', (chunk) => (proc.output += chunk));
  child.on('exit', (code) => (proc.exitCode = code));
  if (stdin !== undefined) child.stdin.end(stdin);
  await waitFor(
    () => proc.output.includes('difit server started') || proc.exitCode !== null,
    'server start',
  );
  if (proc.exitCode !== null) throw new Error(`difit exited with ${proc.exitCode}:\n${proc.output}`);
  return proc;
}

// Closing the tab ends the heartbeat stream, so difit exits and prints the
// review comments.
async function expectExitWithOutput(proc, expected) {
  await waitFor(() => proc.exitCode !== null, 'difit to exit after the browser closed');
  if (proc.exitCode !== 0) throw new Error(`difit exited with ${proc.exitCode}:\n${proc.output}`);
  for (const text of ['Client disconnected, shutting down server...', ...expected]) {
    if (!proc.output.includes(text)) throw new Error(`Output is missing "${text}":\n${proc.output}`);
  }
}

// withPage runs fn with a page in a fresh browser, then closes the browser.
async function withPage(name, fn) {
  const launchOptions = {};
  if (process.env.HTTPS_PROXY) {
    launchOptions.proxy = { server: process.env.HTTPS_PROXY, bypass: process.env.NO_PROXY };
  }
  const browser = await chromium.launch(launchOptions);
  const errors = [];
  try {
    const page = await browser.newPage();
    page.on('pageerror', (error) => errors.push(`pageerror: ${error.message}`));
    page.on('console', (message) => {
      if (message.type() === 'error') errors.push(`console: ${message.text()}`);
    });
    await fn(page);
    if (screenshotDir) {
      await page.screenshot({ path: path.join(screenshotDir, `${name}.png`), fullPage: true });
    }
  } finally {
    await browser.close();
  }
  if (errors.length > 0) throw new Error(`Browser errors:\n${errors.join('\n')}`);
}

async function addCommentInUI(page, lineText, body) {
  const row = page.locator('[data-diff-line-row="true"]', { hasText: lineText }).first();
  await row.hover();
  await row.getByRole('button', { name: 'Add a comment' }).click();
  await page.getByPlaceholder('Leave a comment...').fill(body);
  await page.getByRole('button', { name: 'Submit' }).click();
  await page.getByText(body).first().waitFor();
}

const tests = {
  async 'diff line colors'() {
    const repo = createRepo();
    const difit = await startDifit(repo, []);
    await withPage('diff-line-colors', async (page) => {
      await page.goto(`http://localhost:${difit.port}/`);
      await page.getByText('"Hello, difit"').first().waitFor();

      for (const { theme, addition, deletion } of [
        { theme: 'Dark', addition: 'rgb(18, 38, 30)', deletion: 'rgb(37, 23, 28)' },
        { theme: 'Light', addition: 'rgb(218, 251, 225)', deletion: 'rgb(255, 235, 233)' },
      ]) {
        await page.getByTitle('Settings', { exact: true }).click();
        await page.getByRole('button', { name: theme, exact: true }).click();
        await page
          .getByRole('heading', { name: 'Settings', exact: true })
          .locator('..')
          .getByRole('button')
          .click();

        for (const mode of ['Unified', 'Split']) {
          await page.getByRole('button', { name: mode, exact: true }).click();
          for (const [selector, expected] of [
            ['.bg-diff-addition-bg', addition],
            ['.bg-diff-deletion-bg', deletion],
          ]) {
            await page.waitForFunction(
              ({ selector, expected }) => {
                const elements = [...document.querySelectorAll(selector)];
                return (
                  elements.length > 0 &&
                  elements.every((element) => getComputedStyle(element).backgroundColor === expected)
                );
              },
              { selector, expected },
            );
          }
        }
      }
    });
    await expectExitWithOutput(difit, []);
  },

  // The HEAD commit: a new commit offers a refresh, comments come from both
  // the UI and the CLI, and closing the tab prints them.
  async 'commit review'() {
    const repo = createRepo();
    const difit = await startDifit(repo, []);
    await withPage('commit-review', async (page) => {
      await page.goto(`http://localhost:${difit.port}/`);
      await page.getByText('src/greeting.ts').first().waitFor();
      await page.getByText('"Hello, difit"').first().waitFor();
      await page.getByLabel('1 additions and 1 deletions').waitFor();

      write(repo, 'src/greeting.ts', 'export const greeting = "Hello, watcher";\n');
      git(repo, 'commit', '-q', '-am', 'greet the watcher');
      await page.getByTitle('New commits available - Click to refresh').click();
      await page.getByText('"Hello, watcher"').first().waitFor();

      await addCommentInUI(page, '"Hello, watcher"', 'Smoke comment from the UI');

      // A comment added from the CLI reaches the open page over SSE.
      execFileSync(binary, ['comment', 'add', '--port', String(difit.port)], {
        input: JSON.stringify({
          type: 'thread',
          filePath: 'src/greeting.ts',
          position: { side: 'old', line: 1 },
          body: 'Smoke comment from the CLI',
        }),
      });
      await page.getByText('Smoke comment from the CLI').first().waitFor();
    });
    await expectExitWithOutput(difit, [
      'src/greeting.ts:L1\nSmoke comment from the UI',
      'Smoke comment from the CLI',
    ]);
  },

  // Working-tree review: the untracked-files prompt adds new files with
  // --intent-to-add, and editing a file offers a refresh.
  async 'working tree'() {
    const repo = createRepo();
    write(repo, 'notes/todo.md', '- write tests\n');
    const difit = await startDifit(repo, ['working'], { stdin: 'y\n' });
    if (!difit.output.includes('✅ Files added with --intent-to-add')) {
      throw new Error(`Untracked files were not added:\n${difit.output}`);
    }
    if (git(repo, 'status', '--porcelain') !== 'A notes/todo.md') {
      throw new Error(`Unexpected status: ${git(repo, 'status', '--porcelain')}`);
    }
    await withPage('working-tree', async (page) => {
      await page.goto(`http://localhost:${difit.port}/`);
      await page.getByText('notes/todo.md').first().waitFor();
      await page.getByText('write tests').first().waitFor();

      write(repo, 'src/greeting.ts', 'export const greeting = "Hello, working tree";\n');
      await page.getByTitle('File changes detected - Click to refresh').click();
      await page.getByText('"Hello, working tree"').first().waitFor();

      await addCommentInUI(page, 'write tests', 'Which tests?');
    });
    await expectExitWithOutput(difit, ['notes/todo.md:L1\nWhich tests?']);
  },

  // --background detaches a server that outlives the browser.
  async background() {
    const repo = createRepo();
    const port = await freePort();
    const info = JSON.parse(
      execFileSync(binary, ['--background', '--no-open', '--port', String(port)], {
        cwd: repo,
        env: { ...process.env, DIFIT_CONFIG_DIR: tempDir('difit-e2e-config-') },
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'pipe'],
      }),
    );
    cleanups.push(() => isRunning(info.pid) && process.kill(info.pid));
    if (info.port !== port || !info.url.endsWith(`:${port}`) || !isRunning(info.pid)) {
      throw new Error(`Unexpected background info: ${JSON.stringify(info)}`);
    }
    await withPage('background', async (page) => {
      await page.goto(info.url);
      await page.getByText('"Hello, difit"').first().waitFor();
      await addCommentInUI(page, '"Hello, difit"', 'Left in the background');
    });
    await new Promise((resolve) => setTimeout(resolve, 1000));
    if (!isRunning(info.pid)) throw new Error('Background server exited when the browser closed');
    const comments = execFileSync(binary, ['comment', 'get', '--port', String(port)], {
      encoding: 'utf8',
    });
    if (!comments.includes('Left in the background')) {
      throw new Error(`Comment missing from the background server:\n${comments}`);
    }
    process.kill(info.pid);
    await waitFor(() => !isRunning(info.pid), 'the background server to stop');
  },
};

async function main() {
  const only = process.env.E2E_ONLY;
  for (const [name, test] of Object.entries(tests)) {
    if (only && name !== only) continue;
    const started = Date.now();
    await test();
    console.log(`ok   ${name} (${((Date.now() - started) / 1000).toFixed(1)}s)`);
  }
  console.log('E2E smoke tests passed');
}

main()
  .catch((error) => {
    console.error(error);
    process.exitCode = 1;
  })
  .finally(() => {
    for (const cleanup of cleanups.reverse()) {
      try {
        cleanup();
      } catch {
        // best effort
      }
    }
  });
