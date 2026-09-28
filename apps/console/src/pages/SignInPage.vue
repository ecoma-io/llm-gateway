<script setup lang="ts">
// The sign-in screen, and the only screen a visitor without a session reaches
// (ADR 0012 §2).
//
// The form asks for three things in the order the contract discriminates them:
// the account's id, then the email resolved within it, then the credential
// checked against the row that pair names. An "email first, choose the account
// after" flow would have nothing to verify between the two, so its chooser
// would sit BEFORE authentication and answer "which accounts does this address
// belong to" to anyone who knows the address. The order here is the form's
// order because it is the resolution's order.
//
// There is no field showing whether an account exists and no field showing
// whether an address is in it. The contract gives ONE failure for all of
// "no such account", "no such user", "wrong credential" and "invited row", and
// this screen renders that one answer — a screen that distinguished them would
// be a membership oracle for the account id.
//
// The credential is a `<input type="password">` with `autocomplete` set, held in
// a local ref and cleared the moment the request returns. It is never put in the
// route, never in a store, and never in `localStorage` — the browser's own
// password manager is the right place for a password and this component is
// explicitly not it.
import { Alert, Button, Card, Field, Stack, TextField } from "@ecoma-io/loom";
import { computed, nextTick, ref, useTemplateRef } from "vue";
import { useRoute, useRouter, RouterLink } from "vue-router";

import { signInWith } from "@/lib/api";
import { behaviourFor } from "@/lib/failure-matrix";
import { LANDING_PATH, SIGN_IN_PATH } from "@/router";
import { useSessionStore } from "@/stores/session";
import type { SessionPrincipal } from "@/stores/session";

const router = useRouter();
const route = useRoute();
const session = useSessionStore();

const accountId = ref("");
const email = ref("");
const password = ref("");

const submitting = ref(false);
const failure = ref<{ title: string; message: string } | undefined>(undefined);

/**
 * The id the alert publishes and the form points its own `aria-describedby` at.
 *
 * The alert is a REGION and not an error message for a field, and the two
 * things are genuinely different. The contract answers every wrong input with
 * one 401 that does not say which of the three was wrong, so there is no field
 * whose value was rejected — there is a form that was refused — and
 * `aria-invalid` on any input would be the console guessing, which is the
 * membership oracle the single failure exists to prevent.
 *
 * What the reader still needs is to FIND the answer once it is there, and
 * `role="alert"` alone does not do that: an alert is announced when it appears
 * and is not in the form's own description afterwards, so a reader who is
 * already on the third field, or who looks back at the form rather than
 * listening, never hears why the button did nothing. Describing the FORM by the
 * alert is the fix that claims nothing — it says the form has a reason
 * attached, which is true, and it names no field.
 */
const FAILURE_ALERT_ID = "sign-in-failure";
const form = useTemplateRef<HTMLFormElement>("form");

/**
 * Put the cursor where the answer to the form is, once there is one.
 *
 * The FORM, and never a field. Parking the cursor on the first blank input
 * would assert which of the three values the server rejected, and the server
 * said nothing of the kind: one 401 covers a missing account, an address not
 * in it, a wrong credential and an invited row. A cursor on the account field
 * would answer "that account is not here" and one on the email field would
 * answer "that address is not in that account" — the membership oracle this
 * screen exists to refuse, handed over by the focus ring. Naming a REGION is
 * the only claim here the contract supports. `tabindex="-1"` on the form is
 * what makes that possible without adding a stop to the Tab order, and the
 * reason this is not the account field is written down rather than left to the
 * next person, who will have the same idea.
 */
async function announceFailure(): Promise<void> {
  await nextTick();
  form.value?.focus();
}

/**
 * The one sentence every refused sign-in gets.
 *
 * The matrix's `unauthenticated` row says "Your session has ended", which is
 * right for a session that ended mid-visit and wrong for a credential that was
 * never right. Sign-in is the one screen that renders its own failure title for
 * that reason, and it changes the SENTENCE while taking its `showRequestId`
 * decision from the same row — a wrong credential is not something an operator
 * quotes a request id about, and a 401 here is not something to retry blindly.
 */
const refusedTitle = "Those details did not sign anyone in";
const refusedMessage =
  "Check the account id, the address and the credential. If all three are right, the identity may not be active.";

const canSubmit = computed(
  () => accountId.value.trim() !== "" && email.value.trim() !== "" && password.value !== "",
);

// The guard redirects an already-signed-in visitor away from this form, but a
// screen may be mounted while the guard is still resolving. The flag is a class
// instance rather than a token, a cookie read, or a storage lookup, so there is
// nothing here that could be leaked — and it makes the submit button honest
// rather than inviting a click that would bounce straight back.
const alreadySignedIn = computed(() => session.signedIn);

/** Where to go after signing in: the route the guard bounced, when it is one of
 * ours, and the landing screen otherwise.
 *
 * The check is deliberate rather than a blind redirect. An open redirect through
 * a query parameter is a real vulnerability, and a `redirect` that names
 * another origin would hand a phishing page the operator's trust and the
 * impression that this console sent them there.
 */
function destinationAfterSignIn(): string {
  const asked = route.query.redirect;
  const target = typeof asked === "string" ? asked : "";
  if (!target.startsWith("/") || target.startsWith("//")) return LANDING_PATH;
  if (target.startsWith(SIGN_IN_PATH)) return LANDING_PATH;
  return target;
}

async function submit() {
  if (!canSubmit.value || submitting.value) return;
  submitting.value = true;
  failure.value = undefined;
  try {
    const result = await signInWith({
      account_id: accountId.value.trim(),
      email: email.value.trim(),
      password: password.value,
    });

    // The credential is erased the moment the request is answered, on every
    // path. A failed sign-in that left it in the form would leave it in the DOM
    // of a page an operator walks away from, and it is not a field worth
    // re-typing only because the first attempt had a typo in the account id.
    password.value = "";

    if (!result.ok) {
      const behaviour =
        result.failure.kind === "api"
          ? behaviourFor(result.failure.envelope.error.code)
          : undefined;
      // Both arms are the SAME sentence, deliberately. Every refused sign-in
      // reads identically — "no such account", "no such user", "a wrong
      // credential" and "an invited row" are one 401 by design, and a screen
      // that told them apart would be a membership oracle for the account id.
      // So the title and the message are chosen together, and the code is read
      // for its RECOVERY only: a code whose recovery is "sign-in" is one this
      // screen is the answer to, and anything else is still a refused sign-in
      // rather than a different failure needing different words.
      //
      // The earlier version of this put the matrix's title into the message slot
      // whenever the row said to show a request id — which rendered "Something
      // went wrong on our sideSomething went wrong on our side" inside one
      // alert, because the title was already the heading above it. A request id
      // is not rendered here at all, and deliberately: a wrong credential is not
      // something an operator quotes a request id about, and an id on the
      // sign-in form is an identifier for an attempt that never authenticated
      // anybody.
      const isSignInRecovery = behaviour?.recovery === "sign-in";
      failure.value = {
        title: isSignInRecovery ? refusedTitle : (behaviour?.title ?? refusedTitle),
        message: refusedMessage,
      };
      void announceFailure();
      return;
    }

    const principal: SessionPrincipal = {
      class: result.data.principal.class,
      accountId: result.data.principal.account_id,
      ...(result.data.principal.email === undefined ? {} : { email: result.data.principal.email }),
    };
    session.remember(principal);
    // `push`, not `replace`: a browser Back to this form would be Back to a
    // form for a session the visitor already holds, and the guard would bounce
    // them forward again — a loop. Replacing makes the sign-in the end of that
    // history rather than a step in it.
    await router.push(destinationAfterSignIn()).catch(() => undefined);
  } finally {
    submitting.value = false;
  }
}
</script>

<template>
  <Stack gap="lg" class="mx-auto max-w-md py-8">
    <!--
      The guard sends an already-signed-in visitor away from this route, so this
      arm is only reachable while a session check is still in flight. It says so
      rather than rendering a form whose only action is to bounce.
    -->
    <Card v-if="alreadySignedIn" title="You are already signed in">
      <p class="text-sm text-muted-foreground">
        This session is already established. Go to the dashboard, or sign out first to change which
        identity you are using.
      </p>
      <RouterLink
        to="/"
        class="mt-4 inline-flex min-h-11 items-center rounded-md bg-primary px-4 text-sm font-medium text-primary-foreground focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring"
      >
        Go to the dashboard
      </RouterLink>
    </Card>

    <Card v-else title="Sign in" description="Sign in to the console for your account.">
      <form
        ref="form"
        novalidate
        tabindex="-1"
        :aria-describedby="failure ? FAILURE_ALERT_ID : undefined"
        class="flex flex-col gap-4"
        @submit.prevent="submit"
      >
        <!--
          No per-field error is rendered, and that is the design rather than an
          omission. The contract answers every wrong input with one 401 that
          does not say which of the three was wrong, so a field-level message
          would be the console guessing which one it was — and a guess here is
          the membership oracle the contract's single failure exists to prevent.
          What the form does instead is leave `novalidate` on, so a browser's
          own required-field bubble never claims to know either, and describe
          the failure once, where the answer to it is the same for all three.
        -->
        <Field
          label="Account id"
          hint="The id of the account you are signing in to. It is not the account's name."
          required
        >
          <TextField
            v-model="accountId"
            name="account-id"
            autocomplete="off"
            autocapitalize="off"
            spellcheck="false"
            required
          />
        </Field>

        <Field label="Email" hint="The address of the identity inside that account." required>
          <TextField v-model="email" name="email" type="email" autocomplete="username" required />
        </Field>

        <Field label="Password" required>
          <TextField
            v-model="password"
            name="password"
            type="password"
            autocomplete="current-password"
            revealable
            required
          />
        </Field>

        <!--
          The id is MINE, not Loom's: Loom's `Alert` publishes a `role` and a
          tone and nothing this form can point at, so the wiring the form needs
          has to hang off an element this screen owns. `role="alert"` is left
          as Loom renders it, and is a real difference from the form's
          description: an alert interrupts a reader who is anywhere on the page,
          whereas the `aria-describedby` above is what a reader who returns to
          the form is told about it by.
        -->
        <Alert v-if="failure" :id="FAILURE_ALERT_ID" variant="warning" :title="failure.title">
          {{ failure.message }}
        </Alert>

        <Button type="submit" :loading="submitting" :disabled="!canSubmit" class="min-h-11">
          Sign in
        </Button>
      </form>
    </Card>
  </Stack>
</template>
