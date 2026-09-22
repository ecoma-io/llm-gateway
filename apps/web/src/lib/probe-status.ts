// The contract types a probe's `status` as a bare string, so the badge
// variant must not assume the value: only a confirmed "ok" wears the success
// color. Any other status the gateway reports — or the absence of one — takes
// the destructive variant, which is the honest default until this mapping is
// taught what a newly contracted value means.
import type { HealthStatus } from "@/lib/api";

export function probeStatusVariant(
  status: HealthStatus["status"] | undefined,
): "success" | "destructive" {
  return status === "ok" ? "success" : "destructive";
}
