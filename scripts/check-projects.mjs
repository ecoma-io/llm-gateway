// Asserts every directory under `apps/` is a project Moon can see and that CI
// runs targets for. The mirror of archkeep's `check-packages.mjs`, reduced to
// what this repository's shape needs.
//
// Why this exists — three states of `apps/` produce an identical exit 0 out of
// `moon run ...:lint` and friends:
//
//   1. Nothing is there. Moon prints nothing to run and exits 0.
//   2. A project is there but declares none of the four targets. It is skipped
//      in silence — no warning, no line in the summary.
//   3. A directory is there with sources but no `moon.yml`. It is invisible to
//      Moon entirely; nothing is even skipped, because as far as Moon is
//      concerned nothing exists.
//
// States 2 and 3 are the ones that cost you: the build stays green and nobody
// is told an app is not being checked. This script turns both into a red
// check — it runs in CI (`.github/workflows/ci.yml`), in the pre-commit hook
// (`lefthook.yml`), and nothing else in the repository duplicates it.
//
// What it deliberately does NOT do: parse YAML. The `moon.yml` files this
// repository carries are handwritten with a stable two-space shape, and the
// scan below asserts the four task keys at that indent plus an `id:` matching
// the directory's name — which is what the CI rosters (`web:*`, `api:*`) and
// the workspace globs in `.moon/workspace.yml` route on. A real YAML parser
// would be a dependency bought to re-state those two facts.

import { readdir, readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const appsDir = join(root, "apps");
const requiredTargets = ["lint", "test", "typecheck", "build"];

const entries = await readdir(appsDir, { withFileTypes: true });
const appDirs = entries.filter((entry) => entry.isDirectory()).map((entry) => entry.name);

if (appDirs.length === 0) {
  console.log("0 apps — declared empty");
  process.exit(0);
}

let failed = false;

for (const app of appDirs.sort()) {
  const moonYmlPath = join(appsDir, app, "moon.yml");
  let moonYml;
  try {
    moonYml = await readFile(moonYmlPath, "utf8");
  } catch {
    console.error(`fail ${app} — no moon.yml; Moon cannot see this directory at all`);
    failed = true;
    continue;
  }

  const id = /^id: (\S+)$/m.exec(moonYml)?.[1];
  if (id !== app) {
    console.error(
      `fail ${app} — moon.yml id "${id ?? "(absent)"}" does not match the directory name ` +
        `"${app}"; the CI rosters and the workspace globs route on that identity`,
    );
    failed = true;
    continue;
  }

  const missing = requiredTargets.filter(
    (target) => !new RegExp(`^  ${target}:`, "m").test(moonYml),
  );
  if (missing.length > 0) {
    console.error(`fail ${app} — moon.yml declares no ${missing.join(", ")} target`);
    failed = true;
    continue;
  }

  console.log(`ok   ${app} — ${requiredTargets.join(", ")}`);
}

process.exit(failed ? 1 : 0);
