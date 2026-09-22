// The console consumes only the generated package: no API schema is copied
// into this app. The browser uses the console's own origin — the production
// topology, and the development one too through the dev server's probe proxy
// (vite.config.ts `server.proxy`), so `pnpm dev:api` + `pnpm dev:web` need no
// configuration. `VITE_API_BASE_URL` stays as the optional seam for pointing
// the console at a gateway on another origin.
import {
  client,
  getHealth,
  getReadiness,
  type HealthStatus,
} from "@ecoma-io/llm-gateway-api-client";

const apiBaseUrl = import.meta.env.VITE_API_BASE_URL ?? window.location.origin;

client.setConfig({ baseUrl: apiBaseUrl });

export { getHealth, getReadiness, type HealthStatus };
