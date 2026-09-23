// The package's public surface: a curated re-export of the generated one.
// Consumers see the operations the console actually calls, their option and
// response types, and the shared client instance — never the generator's file
// layout, which is free to change with a pinned version bump. Anything not
// listed here is not API.
//
// `getVersion` is generated and deliberately NOT re-exported. That is not an
// oversight to correct: the operations published here are behaviour this
// package then owes its consumer, and an export nobody imports is a promise
// kept for a caller who does not exist. `/version` remains reachable through
// the generated SDK, so nothing is lost — it becomes public API the day the
// console probes it, which is a one-line change.
//
// The types are held to a different standard on purpose. `HealthStatus` is
// published although only the probes above return it, because a type is the
// contract's vocabulary rather than a behaviour: a consumer that must name a
// shape should be able to name the contract's, not a hand-written copy of it.
export { client } from "./generated/client.gen";
export { getHealth, getReadiness } from "./generated/sdk.gen";
export type {
  GetHealthData,
  GetHealthResponse,
  GetReadinessData,
  GetReadinessResponse,
  HealthStatus,
  Options,
} from "./generated";
