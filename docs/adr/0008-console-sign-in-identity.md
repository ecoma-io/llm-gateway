# ADR 0008: Console sign-in identity — account-scoped resolution

- Status: Accepted
- Date: 2026-09-25
- Issue: [#52](https://github.com/ecoma-io/llm-gateway/issues/52)

## Context

ADR 0006 §3 gives `console-api` "Identity (account, user, session, RBAC,
ownership)", and the first thing the session half does is resolve the user who
is signing in. Nothing on `main` records how that resolution works — and the
schema has already decided the hard half of it. `control.users` enforces its
live-email uniqueness **per account**
(`migrations/control/000002_identity_foundation.up.sql`,
`users_account_live_email_key`: a partial unique index on
`(account_id, email) WHERE state <> 'removed'`), not globally. That shape is
deliberate, twice over: ADR 0001 rule 2 gives a user exactly one account and
models no cross-account identity, so one human who works in two accounts is two
`User` rows sharing an address; and removal is a state, never a delete, so a
removed row keeps its address in the table while the index frees the address
for a re-invite. The domain spells both out
(`identity.UserRemoved`: "its email may be invited again because the live-email
uniqueness is scoped to non-removed rows").

Two traps follow, both found by the adversarial review recorded in issue #52:

1. **The same address can be live in two accounts.** Sign-in that resolves by
   email alone is not a lookup there — it is a guess between two live
   identities of two different accounts.
2. **A removed row and its re-invited successor collide under an unfiltered
   lookup.** `WHERE account_id AND email` with no live-state filter matches both
   rows; a bare `LIMIT 1` then takes one of them in whatever order the planner
   happens to produce, and can silently resolve the tombstone — a terminal
   identity the domain refuses to resurrect — over the live user who now holds
   the address.

The cost of not deciding now is the same one ADR 0006 §5 named for the
projection: the first session pull request decides it silently, in the wire
contract and the sign-in form, where it is expensive to reverse. No product
surface reaches for a user yet — `api/openapi/console.yaml` is probes
(`/healthz`, `/readyz`, `/version`) only, and no user credential exists
anywhere on `main` — so this is the last moment the decision is free.

## Decision

### 1. Per-account uniqueness stays; sign-in resolves `(account_id, email)`

The schema does not move to global email uniqueness, and no migration
accompanies this decision. Console sign-in resolves the signing-in user by
**`(account_id, email)` over live rows** — the exact key
`users_account_live_email_key` enforces — so the pair matches at most one row
by the engine's guarantee rather than by the query's discipline.

`invited` is a **live** state and is therefore in scope of the lookup: the row
an invitation names is the row its activation turns active, and a sign-in
issued for it is the invitation's own redemption, not a second identity.
Whether a _credential_ may be exercised while the identity is still `invited`
is a decision the session phase records against this one — it is not decided
here, and nothing this record says forecloses either answer. What is decided
here is the narrower claim: an `invited` row resolves to exactly one user, and
a `removed` row resolves to nothing. Removal is terminal, and the domain
refuses to resurrect the identity — `Activate` on a removed user is
`ErrInvalidTransition` — so a removed row must never be handed back as
anybody's principal. Resolution is total by construction: one live row, or
none.

### 2. What the console asks for: the account id, with the email, before any credential

The sign-in form collects **the account's id** and **the email**, and only then
the credential. The account id is the discriminator, and it is chosen because
the alternatives each pay more:

- **It is the only identifier the account has.** As a key, `control.accounts`
  offers `id` and nothing else: the remaining columns are a presentation
  `name`, a lifecycle state and stamps — no slug, no handle. The name is not
  unique, and nothing on `main` makes it immutable, so promoting it to a key
  would create a second uniqueness to maintain for a resolution the id already
  makes total.
- **The credential model forces the order anyway.** No user credential exists
  on `main` yet, but wherever the session phase records one, it belongs to the
  `User` aggregate — one row, one `(account_id, email)` pair (ADR 0001 rule 2).
  A per-row credential cannot be evaluated before its row is named, so an
  "email first, choose the account after" flow has nothing to verify in
  between: its chooser would sit **before** authentication and answer "which
  accounts does this address belong to" to anyone who knows the address. On a
  billing console that is an enumeration leak, and the repository already runs
  the opposite policy — the runtime's key authentication answers all four
  failure causes with one sentence (request-lifecycle step 1), and
  `identity.VerifyCredential` burns the digest comparison before anything else
  so wall time names nothing.
- **It keeps failures uniform.** Until the credential matches, sign-in has one
  unauthenticated answer, indistinguishable across no-such-account,
  no-such-user, removed-user and wrong-credential. The account's own lifecycle
  verdict stays where the API-key path already puts it: **after** the credential
  match, with suspended and closed answered distinctly — `VerifyCredential`'s
  step order, authentication before authorisation, extended to the console's
  user path.

The cost is real and named: a UUID is not a thing an operator remembers. It is
accepted because the two cheap-looking alternatives are a second uniqueness to
maintain or a pre-authentication membership disclosure, and because the id
already travels wherever the account presents itself — an invitation, the
account's own console pages, billing correspondence. How the console surfaces
it (a link from an invite, a remembered value, support-assisted recovery) is
presentation, and `identity` refuses to own presentation the same way
`account.go` refuses to own the name beyond "it must say something".

### 3. The lookup rule: live-filtered, keyed, never first-match

Every Control-Plane lookup that resolves by email obeys one rule, whatever the
feature — sign-in, the invite collision check, a future credential-reset flow,
an operator search:

- **It filters live states.** For users the live vocabulary is `invited` and
  `active`; the filter is `state <> 'removed'`, spelled against the lifecycle
  the schema checks. A removed row resolves nothing, ever: removal is terminal
  and the row's only remaining job is history.
- **It is keyed, or it refuses instead of choosing.** A keyed read —
  `(account_id, email)` — returns zero or one live row, because the partial
  unique index says so. An unkeyed search — email alone, or any wider match —
  returns every live row and is an error unless that set is exactly one. No
  `LIMIT 1`, no `ORDER BY` tiebreak, no "first row": ambiguity is a refusal to
  surface, not a choice to hide.
- **The account's state is not part of the user lookup.** Liveness is the user
  row's own lifecycle question; whether the _account_ may act is the post-
  credential verdict of §2. Folding account state into the lookup would leak it
  before the credential matched.

The invite path already runs this rule end to end —
`identity.ErrUserEmailTaken` surfaces the live pair's collision as "already
used by a live user of this account" — so the rule pins existing behaviour and
binds the features that do not exist yet.

### 4. Reversal is a migration, and re-opens this ADR

If a future product decision moves uniqueness to a global live-email index, or
introduces an account-independent credential — a second aggregate ADR 0001
rule 2 does not model today — that is a new file in `migrations/control/`
(AGENTS.md rule 7), never an edit of `000002_identity_foundation`, and it
amends or supersedes this record rather than narrowing it silently.

## Consequences

- **No migration in either lane.** `users_account_live_email_key` becomes
  load-bearing beyond invitations: it is sign-in's key, and "at most one live
  row" is the engine's promise, not the query's.
- **The sign-in surface, when the session phase lands, carries three inputs** —
  account id, email, credential — and its `console.yaml` entries inherit this
  vocabulary. Nothing is on the wire today.
- **Failure answers stay uniform until the credential matches**, and an
  account's suspended/closed verdict may speak only after it matched. A
  contract or handler that distinguishes "no such account" from "wrong
  credential" before authentication contradicts this record, not just a style
  preference.
- **Every future by-email feature starts from the live filter.** Global
  uniqueness would not remove the rule: a removed row in one account and a live
  row in another still both match an unfiltered `WHERE email`, so trap 2
  survives any uniqueness change. The filter is forever; only the ambiguity of
  trap 1 is a choice.
- **The operator-UX cost is accepted and named.** If real operator pain
  arrives, the answer is a recorded decision — a handle, or an account switcher
  fed by an already-authenticated context — not a quiet schema edit.

## Alternatives considered

- **Global email uniqueness (one new control-lane migration)**: rejected — it
  deletes a modelling capability ADR 0001 rule 2 keeps deliberately (one human
  as a live user of two accounts — a contractor's address at two customers) to
  fix a problem this decision fixes without it, and it does not retire the
  live-filter rule, because removed rows persist and still collide under an
  unfiltered lookup.
- **An account handle or slug as the discriminator**: rejected — no column on
  `control.accounts` is one, so it would invent a second unique, immutable
  account identifier, a migration and a naming policy, to solve a resolution
  problem the id already solves. If operator UX ever demands it, it arrives as
  its own recorded decision.
- **Email first, account chooser second**: rejected — with a per-user
  credential there is nothing to authenticate before the account is named, so
  the chooser sits before the credential and discloses account membership to an
  unauthenticated caller; a chooser fed by an _already-authenticated_ context
  is not sign-in resolution and remains available to the console under §2.
- **Resolve by email alone and refuse when ambiguous**: rejected — the refusal
  either discloses that the address is live in more than one account (the same
  leak the chooser has) or hides behind the uniform answer and leaves exactly
  the operators the schema deliberately allows — a live user of two accounts —
  with a dead end and no path; a form field is how the ambiguity is resolved,
  not an error to discover at submit time.
