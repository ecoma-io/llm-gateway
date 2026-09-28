<script setup lang="ts">
// The one-time secret (ADR 0012 §3). This component holds a credential that
// exists nowhere else, not even temporarily, and every rule below is a
// consequence of that one fact. The sharpest are the four the ADR names
// explicitly, and each is enforced structurally rather than by convention.
//
// **A non-reactive ref, scoped here, cleared on the way out.** The token is
// a `shallowRef` in THIS component's scope: not a Pinia store (where a
// devtools inspector or a state-serialising plugin would read it), not a
// route query (where it would land in history, in a referrer header and in
// the address bar), not a prop (so a parent's re-render cannot outlive the
// view and so no ancestor ever holds it), and never an argument to an error
// reporter. `clear()` runs on dismiss AND in `onBeforeUnmount`, so a
// navigation away from the page does not leave a live credential in a
// component instance that is still reachable from a devtools inspector.
//
// **It is never spoken by a live region.** `aria-live` writes to a screen
// reader's buffer, to live captioning and to a scrollback buffer, so a token
// inside one is a disclosure event — a credential read aloud in a shared
// space. There is deliberately NO live region in this template, and no
// `useAnnounce()` call, and the test asserts the negative.
//
// **It is never a toast.** A secret that vanishes after four seconds is
// unusable, and an operator who missed it has to mint a second key — so the
// bad experience of a toast here is not annoyance but a second live
// credential that nobody holds. The reveal is explicit and stays until
// dismissed.
//
// **The element is a real control with the four attributes that stop a
// browser from silently corrupting the credential.** `readonly` makes it
// focusable, selectable and keyboard-reachable rather than a bare text node
// nobody can select. `autocomplete="new-password"` tells a password manager
// this is not one of its fields, and `spellcheck`, `autocapitalize` and
// `autocorrect` all off are not hygiene: iOS Safari capitalises the first
// segment of a pasted token to `Gw_`, and the server's `ParseToken` refuses
// that with a deliberately indistinguishable `ErrMalformedToken`. It is the
// single most likely real-world failure of this feature and it is invisible
// to a test that does not know it exists.
//
// **Both inputs are named, and neither is named by a prop on `TextField`.**
// Loom's `TextField` declares no `label` and no `hint` — it sets
// `inheritAttrs: false` and splits what falls through onto the `<input>` with
// `useSplitAttrs()`, so a `label` handed to it as a prop is not a name, it is
// a literal `label="…"` attribute sitting in the DOM naming nothing, and axe's
// `label` rule reports it critical. `Field` is the component that HAS those
// props, and it does not stop at rendering them: it renders a real `<label
// for>` and calls `provideFieldContext()`, which `TextField` picks up through
// `useFieldControl()` — `aria-labelledby` onto the control, `aria-describedby`
// onto the hint, one `id` shared by the two. So the wrapper is not a `<label>`
// tag this form could have written itself; it is the wiring that makes the
// hint reachable at all, which is why it is used here rather than a bare
// `aria-label` string that would leave a reader unable to ask what a key name
// is for. `SignInPage` is the model and the two screens now differ in nothing
// else.
import { Alert, Button, Card, CopyButton, Field, Stack, TextField } from "@ecoma-io/loom";
import { onBeforeUnmount, ref, shallowRef } from "vue";

import { createApiKey } from "@/lib/api";
import { behaviourFor } from "@/lib/failure-matrix";

/**
 * The credential, held in a component-scoped `shallowRef` and nowhere else.
 *
 * `shallowRef` rather than `ref` is not a micro-optimisation here: a plain
 * `ref` would wrap the value in a reactive proxy and register it in the
 * component's reactive graph, which is the same graph devtools inspects and
 * the same one any future state-serialising plugin would walk. The string
 * has nothing to deep-proxy, so `shallowRef` is both the honest type for it
 * and the one that keeps it out of that graph.
 */
const secret = shallowRef<string | undefined>(undefined);

/** Whether a secret is currently held. The flag, never the secret. */
const revealed = ref(false);
const displayName = ref("");
const minting = ref(false);
const failure = ref<{ title: string; message: string } | undefined>(undefined);

/**
 * Erase the credential. Called by the dismiss button and by `onBeforeUnmount`
 * — two paths to the same guarantee, because a component that clears on
 * dismiss but not on navigation is a component that leaks on a browser Back
 * button, which is the path an operator actually takes.
 */
function clear() {
  secret.value = undefined;
  revealed.value = false;
}

onBeforeUnmount(clear);

async function mint() {
  minting.value = true;
  failure.value = undefined;
  try {
    const result = await createApiKey(displayName.value);
    if (!result.ok) {
      // The mint is the one write that can be refused for a reason the
      // operator can act on, so the failure matrix decides what is said — a
      // page that decided for itself is the thing the matrix exists to stop.
      // The seam has already parsed the envelope, so the code is read from it
      // directly rather than re-derived from a status number here.
      const behaviour =
        result.failure.kind === "api"
          ? behaviourFor(result.failure.envelope.error.code)
          : undefined;
      failure.value = {
        title: behaviour?.title ?? "The key could not be created",
        // The recovery column decides whether a retry is even offered, and
        // the request id is the correlation an operator quotes to whoever can
        // look the failure up. The server's own cause is never carried here.
        message:
          behaviour?.showRequestId === true
            ? "Quote the request id when you report this."
            : "Check the details and try again.",
      };
      return;
    }
    secret.value = result.data.token;
    revealed.value = true;
  } finally {
    minting.value = false;
  }
}
</script>

<template>
  <Stack gap="md" class="max-w-2xl">
    <Card title="Create an API key" description="Keys are created for this account.">
      <Stack gap="md">
        <Field
          label="Key name"
          hint="A name you will recognise later. It is not a secret and can be the same as another key's."
          required
        >
          <TextField v-model="displayName" required :disabled="revealed" />
        </Field>

        <Button :loading="minting" :disabled="revealed" @click="mint"> Create key </Button>

        <Alert v-if="failure" variant="destructive" :title="failure.title">
          {{ failure.message }}
        </Alert>
      </Stack>
    </Card>

    <!--
      The reveal panel. There is no `aria-live` here and none anywhere above
      it: the token must never reach a live region, because a live region
      speaks to a screen reader's buffer, to live captioning and to a
      scrollback buffer. `aria-live="off"` is stated rather than omitted so the
      next person to add an announcement here has to delete a line to make the
      mistake.
    -->
    <Card
      v-if="revealed && secret !== undefined"
      aria-live="off"
      title="Copy this key now"
      description="This is the only time the key is shown. It is not stored, not logged and cannot be retrieved again — if you lose it, create another key and revoke this one."
    >
      <Stack gap="md">
        <!--
          The control that holds the live credential, and the one the whole
          component exists for. It is named by `Field` for the same reason the
          mint form's is: the reader has to be able to say which field they
          are on before they can copy out of it, and an unnamed input makes the
          copy button the only thing on this panel that announces anything. The
          label is deliberately a description of the CONTENT rather than the
          word "token" — a screen reader announces the name of the field when
          focus lands on it, and this is the one place in the console where a
          reader focusing a control is necessarily about to handle a secret.
        -->
        <Field label="API key">
          <TextField
            :model-value="secret"
            readonly
            autocomplete="new-password"
            spellcheck="false"
            autocapitalize="off"
            autocorrect="off"
            name="api-key-secret"
          />
        </Field>

        <div class="flex items-center gap-2">
          <!--
            CopyButton announces the FACT of copying and never the value —
            that is the property that makes it safe next to a secret. It is
            given the token through `getText` rather than `value` so the
            string is read at click time out of the closure and is not part
            of any prop the button renders.
          -->
          <CopyButton :get-text="() => secret ?? ''" label="Copy key" />
          <Button variant="destructive" @click="clear"> I have copied it — hide the key </Button>
        </div>
      </Stack>
    </Card>
  </Stack>
</template>
