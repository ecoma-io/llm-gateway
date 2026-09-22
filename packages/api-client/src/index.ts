// The package's public surface: a curated re-export of the generated one.
// Consumers see the probe operations, their option/response types, and the
// shared client instance — never the generator's file layout, which is free
// to change with a pinned version bump. Anything not listed here is not API.
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
