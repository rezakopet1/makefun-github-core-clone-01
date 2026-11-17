// Thin launcher for the github-core-clone-01 forge.
//
// The forge itself is a compiled static Go binary committed at the repo root
// as ./forge (built from go/ with CGO_ENABLED=0). This file only starts it,
// forwards signals, and exits with its code. The binary owns the whole
// lifecycle: it binds an ephemeral port, POSTs {"port": N} to
// $MAKEFUN_ENV_URL/register, long-polls GET $MAKEFUN_ENV_URL/done, and exits
// 0 when the env says done. Nothing here touches the network or the store.
import { spawn, type ChildProcess } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const forgePath = path.join(path.dirname(fileURLToPath(import.meta.url)), 'forge');

const child: ChildProcess = spawn(forgePath, [], { stdio: 'inherit' });

for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP'] as const) {
  process.on(sig, () => {
    try {
      child.kill(sig);
    } catch {
      // The child already exited: nothing left to forward to.
    }
  });
}

child.on('error', (err) => {
  process.stderr.write(`[entry] cannot start ./forge: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  if (signal !== null) {
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
