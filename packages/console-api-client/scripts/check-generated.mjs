// Proves src/generated matches the checked-in OpenAPI contract without
// touching it. Moon runs its targets concurrently, so regenerating in place
// races tests/typecheck importing the client; a temporary output instead makes
// freshness a read-only assertion. Both executables are invoked by their known
// workspace paths rather than `pnpm run`: Moon's task environment intentionally
// does not route through a locally installed pnpm/proto toolchain.
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

const packageRoot = resolve(fileURLToPath(new URL("..", import.meta.url)));
const workspaceRoot = resolve(packageRoot, "../..");
const output = await mkdtemp(join(tmpdir(), "llm-gateway-console-api-client-"));

function run(executable, args) {
  return new Promise((resolveRun, reject) => {
    const child = spawn(executable, args, {
      cwd: packageRoot,
      stdio: "inherit",
    });
    child.on("error", reject);
    child.on("close", (code) => {
      if (code === 0) {
        resolveRun();
        return;
      }
      reject(new Error(`${executable} exited with code ${code}`));
    });
  });
}

try {
  await run(join(packageRoot, "node_modules/.bin/openapi-ts"), ["--output", output]);
  await run(join(workspaceRoot, "node_modules/.bin/prettier"), [
    "--config",
    join(workspaceRoot, ".prettierrc"),
    "--write",
    output,
  ]);
  await run("diff", ["--recursive", "--unified", "src/generated", output]);
} finally {
  await rm(output, { force: true, recursive: true });
}
