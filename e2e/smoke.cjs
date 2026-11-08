// End-to-end smoke test: serves a real repository with the difit binary,
// drives the UI in Chromium, and checks watcher reloads, live comment updates
// and the shutdown-on-tab-close behavior.
//
// Usage: node e2e/smoke.cjs [path/to/difit] [screenshot.png]
// Needs the playwright package (NODE_PATH may point at a global install).

const { spawn, execFileSync } = require('node:child_process');
const fs = require('node:fs');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');
const { chromium } = require('playwright');

const binary = path.resolve(process.argv[2] ?? 'bin/difit');
const screenshot = process.argv[3];

function git(cwd, ...args) {
  return execFileSync('git', args, { cwd, encoding: 'utf8' }).trim();
}

function createRepo() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'difit-e2e-'));
  git(dir, 'init', '-q', '-b', 'main');
  git(dir, 'config', 'user.name', 'E2E');
  git(dir, 'config', 'user.email', 'e2e@example.com');
  git(dir, 'config', 'commit.gpgsign', 'false');
  fs.mkdirSync(path.join(dir, 'src'));
  fs.writeFileSync(path.join(dir, 'src/greeting.ts'), 'export const greeting = "Hello";\n');
  git(dir, 'add', '-A');
  git(dir, 'commit', '-q', '-m', 'initial');
  fs.writeFileSync(path.join(dir, 'src/greeting.ts'), 'export const greeting = "Hello, difit";\n');
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

async function main() {
  const repo = createRepo();
  const configDir = fs.mkdtempSync(path.join(os.tmpdir(), 'difit-e2e-config-'));
  const port = await freePort();
  const env = { ...process.env, DIFIT_CONFIG_DIR: configDir };

  let output = '';
  let exitCode = null;
  const difit = spawn(binary, ['--no-open', '--port', String(port)], {
    cwd: repo,
    env,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  difit.stdout.on('data', (chunk) => (output += chunk));
  difit.stderr.on('data', (chunk) => (output += chunk));
  difit.on('exit', (code) => (exitCode = code));
  await waitFor(() => output.includes('difit server started'), 'server start');

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

    await page.goto(`http://localhost:${port}/`);
    await page.getByText('src/greeting.ts').first().waitFor();
    await page.getByText('"Hello, difit"').first().waitFor();
    await page.getByLabel('1 additions and 1 deletions').waitFor();

    // A new commit is picked up by the watcher and offered as a refresh.
    fs.writeFileSync(path.join(repo, 'src/greeting.ts'), 'export const greeting = "Hello, watcher";\n');
    git(repo, 'commit', '-q', '-am', 'greet the watcher');
    await page.getByTitle('New commits available - Click to refresh').click();
    await page.getByText('"Hello, watcher"').first().waitFor();

    // A comment added from the CLI reaches the open page over SSE.
    execFileSync(binary, ['comment', 'add', '--port', String(port)], {
      input: JSON.stringify({
        type: 'thread',
        filePath: 'src/greeting.ts',
        position: { side: 'new', line: 1 },
        body: 'Smoke comment from the CLI',
      }),
    });
    await page.getByText('Smoke comment from the CLI').first().waitFor();

    if (screenshot) {
      await page.screenshot({ path: screenshot, fullPage: true });
    }
  } finally {
    await browser.close();
  }

  // Closing the tab ends the heartbeat stream, so difit exits and prints
  // the review comments.
  await waitFor(() => exitCode !== null, 'difit to exit after the browser closed');
  if (exitCode !== 0) throw new Error(`difit exited with ${exitCode}:\n${output}`);
  for (const expected of ['Client disconnected, shutting down server...', 'src/greeting.ts:L1', 'Smoke comment from the CLI']) {
    if (!output.includes(expected)) throw new Error(`Output is missing "${expected}":\n${output}`);
  }
  if (errors.length > 0) throw new Error(`Browser errors:\n${errors.join('\n')}`);

  fs.rmSync(repo, { recursive: true, force: true });
  fs.rmSync(configDir, { recursive: true, force: true });
  console.log('E2E smoke test passed');
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
