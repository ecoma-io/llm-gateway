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
import { computed, ref } from "vue";
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
      <form novalidate class="flex flex-col gap-4" @submit.prevent="submit">
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

        <Alert v-if="failure" variant="warning" :title="failure.title">
          {{ failure.message }}
        </Alert>

        <Button type="submit" :loading="submitting" :disabled="!canSubmit" class="min-h-11">
          Sign in
        </Button>
      </form>
    </Card>
  </Stack>
</template>
