<script setup lang="ts">
// The ONE way a failure reaches a reader (ADR 0012 §6).
//
// Every screen that can fail renders its failure through this component, and
// this component renders it through `CONSOLE_BEHAVIOUR` — the table keyed
// exhaustively over the contract's `Error["code"]` union. It never re-derives
// a behaviour from a status number, and it never invents a code to look up.
// A page that decided for itself what a 503 means is the second opinion this
// file exists to make impossible.
//
// The four behaviours the table names, and what each becomes here:
//
//   - `title` + `variant` are the `Alert`'s, so a tone here is never the only
//     channel: `Alert` paints an icon and a word, not a colour alone.
//   - `recovery: "sign-in"` is the ONLY behaviour that navigates, and it
//     routes to sign-in. The shell is a router so the button is a
//     `RouterLink` — a real href, so it is keyboard-operable, focusable and
//     a history entry — and this is the one place a failure moves the visitor.
//   - `recovery: "retry"` renders a retry button that calls the screen's
//     `onRetry` prop. A screen passes it; this component does not fetch, and
//     it does not keep the last good data — the screen owns that, because
//     whether stale data stays visible is the screen's call, not the
//     failure's.
//   - `recovery: "none"` offers no button at all. A `500` is not something an
//     operator should try again, and a retry button next to a bug is how
//     operators learn to click through outages.
//
// `showRequestId` decides whether the correlation is surfaced, and the row —
// not the component — decides. `not_found` and `unauthenticated` do not show
// one, because an operator's next move for those is not to quote an
// identifier.
//
// A `TransportFailure` has no code and no envelope. It renders as its own
// honest "we could not reach the service, try again" with a retry, and it
// names NO code — a fabricated `internal` would show a `request_id` nobody
// can look up and a fabricated `upstream_unavailable` would promise a retry
// against an upstream that was never asked. The one thing a caller can do
// about a body that is not a contract body is try again, and that is all it
// says.
//
// The message is spoken by a live region: `live="assertive"` on the `Alert` is
// the interruption, and it carries the *title* and the body — never a secret. No
// credential, token or key material is ever passed to this component, and
// none of the words it renders is one.
//
// The interruption is set through Loom's `live` prop rather than a `role` on
// the `Alert`, and that is a correction rather than a preference. `Alert`
// declares `live` and not `role`, so a `role="alert"` passed here was a
// fall-through attribute that happened to land on the element — it worked only
// because the tone was `destructive` or `warning`, which are Loom's two
// assertive tones, so a row that added a third neutral failure would have
// quietly stopped being announced. `live="assertive"` says it and Loom chooses
// the element that carries it, so the promise survives a row this console did
// not write.
import { Alert, Button, Stack } from "@ecoma-io/loom";
import { computed } from "vue";
import { RouterLink } from "vue-router";

import { CONSOLE_BEHAVIOUR, type CodeBehaviour } from "@/lib/failure-matrix";
import type { Failure } from "@/lib/api";

const props = withDefaults(
  defineProps<{
    /** The failure, as the seam parsed it. Never re-derived here. */
    failure: Failure;
    /**
     * The screen's retry, offered ONLY when the behaviour row says `retry`.
     * A failure whose recovery is `none` never calls it.
     */
    onRetry?: (() => void) | (() => Promise<void>);
    /** Set while the screen's retry is in flight, to lock the button. */
    retrying?: boolean;
  }>(),
  { onRetry: undefined, retrying: false },
);

/**
 * The row this failure renders through, or `null` for a transport failure.
 *
 * A `null` row is the honest answer for a body that never reached the
 * contract's vocabulary — there is no code to look up, and the component
 * declines to look one up.
 */
const behaviour = computed<CodeBehaviour | null>(() =>
  props.failure.kind === "api" ? CONSOLE_BEHAVIOUR[props.failure.envelope.error.code] : null,
);

/** The correlation, shown only when the row says to and only for an API failure. */
const requestId = computed<string | null>(() => {
  if (behaviour.value?.showRequestId !== true) return null;
  if (props.failure.kind !== "api") return null;
  return props.failure.envelope.request_id;
});

const isTransport = computed(() => props.failure.kind === "transport");

/**
 * The headline. An API failure is the row's declared title; a transport
 * failure is its own honest sentence, because it has no row and no code to
 * render one from.
 */
const title = computed(() => behaviour.value?.title ?? "The console could not reach the gateway");

/**
 * The body. Deliberately the console's own sentence about what to do, NOT the
 * server's `message`: the contract says that message is safe to present, which
 * makes it a candidate, not a requirement, and echoing the server's
 * vocabulary verbatim renders it in a place the operator has learned to
 * trust. The request id, when shown, is the thing to quote.
 */
const message = computed(() => {
  if (isTransport.value) {
    return "The response was not something this console knows how to read. Trying again may work.";
  }
  switch (behaviour.value?.recovery) {
    case "sign-in":
      return "Sign in again to continue.";
    case "retry":
      return "The data below is the last that loaded. Try again for the current figures.";
    case "none":
    default:
      return requestId.value
        ? "Quote the request id when you report this — it is how the failure is looked up."
        : "There is nothing to try here. Check the details and try again later.";
  }
});

/**
 * The variant for the `Alert`. A transport failure is `warning` — the same
 * tone the table gives the retryable codes — because the caller's next move
 * is identical: try again.
 */
const variant = computed(() => behaviour.value?.variant ?? "warning");

/** Whether to offer a retry: the row says `retry`, and a transport failure does too. */
const offersRetry = computed(() => isTransport.value || behaviour.value?.recovery === "retry");

function retry() {
  void props.onRetry?.();
}
</script>

<template>
  <Alert :variant="variant" :title="title" live="assertive">
    <Stack gap="sm" class="mt-1">
      <p>{{ message }}</p>

      <p v-if="requestId" class="font-mono text-xs">
        Request id: <span class="break-all">{{ requestId }}</span>
      </p>

      <div class="flex flex-wrap items-center gap-2 pt-1">
        <!--
          A `sign-in` recovery is a navigation, and it is a LINK: a real href
          is keyboard-operable, focusable, and a history entry, which a
          programmatic `router.push` from a click handler is not. It is the
          only behaviour that moves the visitor, and only for `unauthenticated`.
        -->
        <RouterLink
          v-if="behaviour?.recovery === 'sign-in'"
          to="/sign-in"
          class="inline-flex min-h-11 items-center rounded-md bg-primary px-4 text-sm font-medium text-primary-foreground focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-ring"
        >
          Sign in again
        </RouterLink>

        <!--
          The retry affordance. `min-h-11` is WCAG 2.2's 24×24 target floor
          given with room; Loom's `Button` sizes the control, and the class
          keeps the hit area at the AA minimum even at a small size.
        -->
        <Button
          v-else-if="offersRetry && onRetry"
          variant="secondary"
          :loading="retrying"
          @click="retry"
        >
          Try again
        </Button>
      </div>
    </Stack>
  </Alert>
</template>
