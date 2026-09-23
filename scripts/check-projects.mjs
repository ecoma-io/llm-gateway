// Asserts every directory under `apps/` and `packages/` is a project Moon can
// see and declares the four targets CI runs. It deliberately does not parse
// CI's roster: `.github/workflows/ci.yml` names the targets it runs
// explicitly, and a new project has to be added to that roster by hand — this
// script turns an invisible project into a red check, but a roster that
// forgot to grow is a gap it cannot see. The mirror of archkeep's
// `check-packages.mjs`, reduced to what this repository's shape needs.
//
// Why this exists — three states of a workspace root produce an identical
// exit 0 out of `moon run ...:lint` and friends:
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
// the directory's name — which is what the CI rosters (`console:*`,
// `console-api:*`, `dataplane:*`, `dataplane-api:*`, `api-client:*`) and the
// workspace globs in `.moon/workspace.yml` route on. A
// real YAML parser would be a dependency bought to re-state those two facts.

import { readdir, readFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
// The two workspace roots of pnpm-workspace.yaml / .moon/workspace.yml. `apps`
// carries the applications; `packages` carries what they share. A root that does
// not exist yet is an empty roster, not an error — absence of a whole root is
// visible in the tree, unlike a silently-skipped directory inside one.
const workspaceRoots = ["apps", "packages"];
const requiredTargets = ["lint", "test", "typecheck", "build"];

let failed = false;
let projectCount = 0;

for (const workspaceRoot of workspaceRoots) {
  let entries;
  try {
    entries = await readdir(join(root, workspaceRoot), { withFileTypes: true });
  } catch {
    continue;
  }

  const projectDirs = entries
    .filter((entry) => entry.isDirectory())
    .map((entry) => entry.name)
    .sort();

  for (const project of projectDirs) {
    const moonYmlPath = join(root, workspaceRoot, project, "moon.yml");
    let moonYml;
    try {
      moonYml = await readFile(moonYmlPath, "utf8");
    } catch {
      console.error(`fail ${project} — no moon.yml; Moon cannot see this directory at all`);
      failed = true;
      continue;
    }

    const id = /^id: (\S+)$/m.exec(moonYml)?.[1];
    if (id !== project) {
      console.error(
        `fail ${project} — moon.yml id "${id ?? "(absent)"}" does not match the directory name ` +
          `"${project}"; the CI rosters and the workspace globs route on that identity`,
      );
      failed = true;
      continue;
    }

    const missing = requiredTargets.filter(
      (target) => !new RegExp(`^  ${target}:`, "m").test(moonYml),
    );
    if (missing.length > 0) {
      console.error(`fail ${project} — moon.yml declares no ${missing.join(", ")} target`);
      failed = true;
      continue;
    }

    console.log(`ok   ${project} — ${requiredTargets.join(", ")}`);
    projectCount += 1;
  }
}

if (projectCount === 0) {
  console.log("0 projects — declared empty");
}

process.exit(failed ? 1 : 0);
