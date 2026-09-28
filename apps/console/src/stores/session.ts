// The session store holds ONLY what the server told it: a boolean, the
// principal's class, and the account the session acts for. It holds no token,
// no secret and no credential, and it could not if it tried to — the session
// credential is an `HttpOnly` `Set-Cookie` the browser will not hand to
// JavaScript, so the console has no bearer string to keep. That is not a
// limitation worked around; it is the property the whole design rests on
// (ADR 0012 §2): a page that cannot read its own credential cannot leak it
// through a store, a route, a prop or an error reporter.
//
// The one call that establishes "signed in" is `getSession`, and a 200 there
// is treated as proof of exactly that much and no more. The server re-checks
// every product operation regardless, so a stale `true` in this store costs a
// round trip, never a wrong screen.
import { defineStore } from "pinia";
import { computed, ref } from "vue";

import { getSessionResult, signOutOfSession } from "@/lib/api";
import type { PrincipalClass } from "@ecoma-io/llm-gateway-console-api-client";

/**
 * What the console knows about the current session, and the full extent of it.
 *
 * A `class` with no `user_id` and no `operator_id` is a fact about the
 * contract, not an unfinished object: `Principal` carries the identity
 * element exactly when the class has one, and the console renders the class
 * and the account because those are what every product operation authorizes
 * against. The address is held because the signed-in person is already
 * authenticated and it is their own address — it is a label, never a lookup
 * key, and it never appears in a URL.
 */
export interface SessionPrincipal {
  readonly class: PrincipalClass;
  readonly accountId: string;
  readonly email?: string;
}

export const useSessionStore = defineStore("session", () => {
  /**
   * Whether a session is established. `false` before the first `getSession`
   * and after a refusal, `true` only on a 200.
   */
  const signedIn = ref(false);
  const principal = ref<SessionPrincipal | undefined>(undefined);
  /**
   * True while the store holds neither an answer nor an error — the state a
   * route guard waits out before deciding, so a first paint does not bounce an
   * authenticated visitor through the sign-in screen and back.
   */
  const resolving = ref(false);

  const accountId = computed(() => principal.value?.accountId);

  /**
   * Clear the session locally. Deliberately does NOT call the server: this is
   * what a 401 does, and a 401 means the server has already ended the session,
   * so a sign-out call here would be a request whose failure changes nothing.
   */
  function forget() {
    signedIn.value = false;
    principal.value = undefined;
  }

  /**
   * Record what `signIn` returned. The response carries the principal and
   * nothing else — no token, because a JSON body cannot set a cookie.
   */
  function remember(session: SessionPrincipal) {
    principal.value = session;
    signedIn.value = true;
  }

  /**
   * Ask the server who this session belongs to.
   *
   * Returns whether a session exists, and never throws: the caller of a guard
   * wants a boolean, and an exception thrown out of a navigation guard lands
   * the visitor in Vue's own error screen rather than on sign-in. Anything
   * that is not a 200 is "no session" — the contract is explicit that the
   * answer is identical whether the cookie was absent, expired, revoked or
   * never existed, and it is this client's job not to tell those apart.
   */
  async function resolve(): Promise<boolean> {
    resolving.value = true;
    try {
      const result = await getSessionResult();
      if (!result.ok) {
        forget();
        return false;
      }
      // The contract answers `GET /auth/session` with the `Principal` itself
      // rather than a document wrapping one, so the principal IS the body.
      const who = result.data;
      remember({
        class: who.class,
        accountId: who.account_id,
        ...(who.email === undefined ? {} : { email: who.email }),
      });
      return true;
    } finally {
      resolving.value = false;
    }
  }

  /**
   * End the session. The server's call is idempotent, so this is safe to run
   * from a button and from a 401 handler alike; the local state is cleared
   * either way, because leaving a console wearing a signed-in shell after the
   * sign-out failed is the failure mode ADR 0012 §2 names.
   */
  async function end() {
    try {
      await signOutOfSession();
    } finally {
      forget();
    }
  }

  return { signedIn, principal, resolving, accountId, forget, remember, resolve, end };
});
