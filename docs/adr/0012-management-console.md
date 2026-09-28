# ADR 0012: The management console — its surface, its read model, and what it does not have

- Status: Accepted
- Date: 2026-09-27
- Issue: [#120](https://github.com/ecoma-io/llm-gateway/issues/120)
- Builds on: [ADR 0006](0006-control-plane-and-data-plane.md) §3, §6, §9, [ADR 0008](0008-console-sign-in-identity.md)

## Context

`api/openapi/console.yaml` holds three unauthenticated probes. The Control
Plane's other four domains are written and unreachable: `identity`,
`commerce`, `accounting` and `reconciliation` have use cases, ports, schema
and tests, and no operation that a browser can call. The console itself is one
page showing probe status.

A product change was proposed to establish the management console: a
dashboard, identity management with one-time secret rendering, a model
catalog, commerce and accounting views, a request explorer, runtime views and
a reconciliation view. Seven adversarial reviews ran against it before any code
was written, and they did not agree that it is one change. They agreed
independently on the following, which is why this record exists.

### What the reviews found, stated as the code sees it

**1. The framing was wrong.** The proposal called `console-api` "a BFF façade,
not a domain owner". ADR 0006 §3 assigns `console-api` both: it owns five
domain packages and the `control` database, _and_ "console orchestration". §9's
word "façade" belongs to `dataplane-api`, whose constraint is that it owns no
domain — §9's whole argument is that `dataplane-api`'s package set is
deliberately smaller than `console-api`'s. Reading façade onto `console-api`
inverts the asymmetry. The two roles are real and they are not the same role,
and `internal/arch` encodes the difference: `internal/adapters/inbound/http`
may import `internal/application` and may **not** import `internal/domain`
(`imports_test.go:105-114`). A handler that joins use cases must therefore
render wire shapes of its own and may not touch an ownership aggregate. That
is the boundary, and it is already drawn.

**2. There is no read port for almost any screen.** The persistence ports
declare only what a use case has already asked for — `FundingBuckets.Sweep`'s
doc at `persistence/accounting.go:91`: "a member is added when a consumer
asks for it, not before". `Accounts` has
`Create`, `ByID`, `TransitionState`. `Users` has no `ByAccountAndEmail`.
`FundingLedger` has no list of any kind. `ReconciliationFindings` has
`Open`, `Restatus`, `OpenCount` — a count and no rows. `Settlements.Recent` is
documented as a deliberate absence. Every list a console shows is a new port
member, and thirteen of the fifteen Control Plane resources need one.

**3. Authorization does not exist, and the product surface is unauthenticated.**
`console.yaml` has no `securitySchemes` and no `security:` block. The only
middleware is `requestID`. The `route` struct is `{method, path, handler}` with
no field to hang a guard on. `control.users` has no credential column. There is
no session table, no role, and `internal/domain` contains no `Role` type at
all — ADR 0006 §3 promises RBAC and none of it is built. An unauthenticated
`POST /accounts/{id}/api-keys` would mint live credentials, and every existing
check in this repository is silent on that: no test asserts an endpoint is
guarded, because there is no guard to assert. A console without a session
surface is not a console with a security gap; it is a console that cannot ship.

**4. Three of the proposed features have no contract anywhere.** The model
catalog, the request explorer and the runtime views are all Data Plane state.
`api/openapi/dataplane.yaml` declares five operations: `listUsageEvents`,
`getCurrentAliasGroupVersion`, `getProjectionPosition`,
`applyProjectionSnapshot`, `applyProjectionChanges`. None lists a request, a
provider, an alias or a route. The outbound port is seven methods
(`WithdrawCredential` — declared and callerless — `CurrentGroupVersion`,
`ReadUsageEvents`, `ProjectionPosition`, `DeliverSnapshot`, `DeliverChanges`),
and `GroupVersion` carries three scalars and no model. `public.requests` has a
primary key and no other index in the entire repository. Those screens are
three layers of work in two Go modules and a contract the proposal never
mentions.

**5. Money has no currency and no way to be summed.** `domain/accounting/money.go`:
amounts are integer minor units and "the single platform-wide settlement
currency is configuration that lives above this package" — there is no
currency field on a bucket, a settlement, a ledger entry or a plan version, and
no currency type anywhere. A console that renders `$` has invented a fact. The
three cached balances are a projection ("if the cache and the legs ever
disagree, the legs win"), and no read port returns a leg list to sum, so a page
total over a partial keyset is wrong twice over: the page is not the ledger,
and a `Σ amount` across six leg kinds is not money.

**6. Reconciliation has no repair path and must not grow one.**
`ports/outbound/persistence/reconciliation.go:12-13`: "What this port is NOT,
and cannot be made into by a future change to it, is a repair path." `Restatus`
"deliberately cannot be a repair". A console that offers _refund_ on a finding
invents an operator identity — and
`internal/domain/accounting/ids.go:55-58` records why that is a grammar
nobody has set: `OperatorID` is "Text, not a users-row reference: the operator
surface is not built yet, and the ledger must not guess at its grammar". The
value is a permanent, un-retractable attribution on an append-only
financial record.

**7. Seven of eleven contract error codes have no code path in `console-api`.**
`shared/errors.yaml` declares eleven. `application.Code` has two, and
`errorResponse` produces four. `invalid_request`, `unauthenticated` and
`cursor_expired` are all required by the product surface and none is
implemented — so the API-failure matrix is not a test to write, it is
behaviours to design.

**8. The failure matrix has one entry that is a bug, not a state.**
`upstream_unavailable` is `503` with a retry affordance; `internal` is `500`
with none. A console that renders both as "something went wrong" trains
operators to ignore outages and hides a real one behind a shrug. And the seven
existing list surfaces have no envelope at all — `GatewayStatusPage.vue:87`
renders the raw contract token `ok` as the badge's text.

**9. A real defect, filed as [#116](https://github.com/ecoma-io/llm-gateway/issues/116).**
`reconciliation_findings` has no run column, though its own migration header
says it does.

## Decision

### 1. `console-api` holds both roles, and BFF composition is a use case

`console-api` is a domain owner **and** the console's orchestrator. Where a
screen joins more than one domain's answer, that join is a use case in
`internal/application` returning a projection, with its own operation in
`console.yaml` — not arithmetic in a page component, and not a new layer.
`internal/bff` is not created: `packages_test.go:139-166` fails any package
outside the sanctioned roots, and a new one is "a deliberate change to ADR
0006 §6".

The route table is split per bounded context, each function taking only the
use cases it serves, composed by `routes()`. `contract_test.go` keeps working
unchanged because it iterates `routes()`. `application.App` grows one narrow
field per context, and `New` panics on a nil port exactly as `NewIdentity`
does — with a test-local constructor so `contract_test.go` never grows a
database dependency.

### 2. The session surface ships first, in its own change, before any product endpoint

No management operation lands before sign-in, sessions and server-side
authorization. This is not a feature preference: an unauthenticated
`POST /accounts/{id}/api-keys` mints a live credential and nothing in this
repository would notice.

What that surface decides, and what it does not:

- **Sign-in resolution is ADR 0008's**, unchanged: `(account_id, email)` over
  live rows, three inputs, one uniform failure answer until the credential
  matches, and the account's suspended/closed verdict only after.
- **The credential is PBKDF2-HMAC-SHA256, from `crypto/pbkdf2` in the standard
  library.** No new dependency. A user credential is a password, and the API
  key path already established that a digest — not the secret — is what
  persists. What ADR 0008 left open, whether an `invited` identity may exercise
  a credential, is decided here: **it may not.** Invitation is a state the
  record-keeping flip produces, and a credential is a grant of access;
  conflating them means a leaked invitation email is access.
- **The session is opaque, server-side, and stored hashed.** A 256-bit random
  id, SHA-256 at rest, in `control.sessions` (next free number: `000010`).
  Opaque rather than a signed token, so logout is immediate and revocable
  rather than "valid until it expires". Rotated on every privilege change.
  Delivered `HttpOnly; Secure; SameSite=Strict; Path=/`.
- **`account_id` is taken from the session, never from the request, and never
  from a path.** ADR 0001 rule 2 gives a user exactly one account; a
  path-scoped `/accounts/{account_id}/...` would write every operator's
  reading of every customer's ledger into every reverse proxy's access log and
  into the `Referer` of every outbound link. The account is implied by the
  session: `/api/me`, `/api/me/api-keys`, `/api/me/ledger`.
- **A resource that is not the principal's is `404`, never `403`.** `403` is a
  confirmation oracle. The persistence predicate is part of the query
  (`WHERE id = $1 AND account_id = $2`) so both cases return zero rows at the
  same cost, which is what makes the timing indistinguishable.
- **The console never accepts a `gw_` API key.** `identity.Principal` already
  models the two kinds and requires a reader to switch on `Kind`; a runtime
  credential is not a console identity, and `dataplane.yaml`'s `Unauthenticated`
  description already says a browser session and a customer key are both
  refused there for exactly this reason.
- **Two classes, not a role model.** `user` and `operator`, as a column set at
  creation from the server's own decision and never read from a request. A
  three-role RBAC system invented inside an implementation change is a role
  model nobody reviewed, and the migration to change it later runs over live
  authorization decisions.
  These are **classes of a holder, not kinds of a credential**, and the
  distinction is structural rather than a naming preference. `identity.Kind`
  already exists and is the orthogonal axis: it says _which credential
  authenticated_ — `PrincipalUser` for a console identity, `PrincipalAPIKey` for
  a runtime credential. A session is a browser credential, so its `Kind` is
  always `PrincipalUser`; the class says _what that holder may do across
  accounts_ (`user` acts for exactly one, `operator` administers several).
  Conflating them would either invent a third credential kind the runtime has
  no way to present, or demote `user`/`operator` to a redeclaration of what
  `Kind` already says. So the two live on different types:
  `identity.SessionClass` is the holder's authorisation class, validated to
  its own vocabulary, and the session's own aggregate — not `Principal` —
  carries it. `operator` is admitted in the column's vocabulary and in the
  contract enum now, so adding the staff surface later is a new predicate and
  not a migration over live authorization decisions, which is the whole reason
  this record refuses a three-role model. The `operator_id` it names is text,
  not a `users` row, and the reason is the same one
  `domain/accounting/ids.go:55-58` gives: the operator surface is not built and
  the grammar of a staff identity is not something the ledger may guess at.
  **`user` is the only class the first console mints.** A row cannot be
  created as `operator` through any surface this change ships, and the
  predicate for an operator session is not yet written — so a class that has no
  predicate is unreachable rather than merely unused, and the one test worth
  having here is that no signed path produces one.
- **The browser is not a security boundary, and the mechanisms are named.** No
  credential in `localStorage`, `sessionStorage` or `IndexedDB` — with one
  named exception already in the tree, `loom:theme` in
  `apps/console/index.html:16`, which is a theme preference and not a
  credential. Beyond the cookie: an origin check on every unsafe method
  (`Sec-Fetch-Site` first, `Origin` second), `application/json` required and
  nothing else accepted, and a double-submit token in a header. Four layers,
  because `SameSite` is a browser control and not a boundary.

### 3. The one-time secret

Rendered once, never persisted, never logged, and now enforced rather than
intended:

- The mint response is a hand-written DTO with a bespoke `MarshalJSON`.
  `application.MintedKey` refuses serialisation and **stays refusing** — a
  BFF that needs a token on the wire copies the string at the boundary and
  never weakens the refusal.
- `Cache-Control: no-store` on every product response. `writeJSON` sets only
  `Content-Type` today; the Data Plane's runtime already sends `no-cache` and
  the Control Plane is the weaker of the two.
- No request or response body is ever logged. The one correlation handle is
  `X-Request-Id`, which is grammar-checked on ingress and therefore cannot
  carry a log injection — and which is a correlation, never an audit
  identity, because a caller may choose it.
- The frontend holds it in a non-reactive ref scoped to the reveal component,
  cleared on unmount, never in a store, a route, a prop or an error reporter.
- The element is a real control: `readonly`, `autocomplete="new-password"`,
  `spellcheck="false"`, `autocapitalize="off"`, `autocorrect="off"`. iOS
  Safari capitalising the first segment to `Gw_` on copy produces a token
  `ParseToken` refuses with the deliberately indistinguishable
  `ErrMalformedToken` — the single most likely real-world failure of this
  feature, and invisible to a test that does not know it exists.
- **It is never spoken by a live region.** `aria-live` writes to a screen
  reader's buffer, to live captioning, and to a scrollback buffer. A
  one-time secret that is `aria-live` is a disclosure event. `CopyButton`
  announces the fact of copying and never the value; that stays.
- **It is never a toast.** A secret that vanishes after four seconds is
  unusable and forces re-creation, which mints a second live credential.
- Minting has no idempotency key, so a browser resubmission after a
  user-visible error would mint a second key whose secret nobody holds. The
  one-time secret is shown once, the _replay_ is refused: the API-key
  ownership record is a first creation, and a retry is a new mint the operator
  has to revoke. Named here so it is a decision, not an accident.

### 4. The read model: one port member per screen, and the money rules

Every list is a new method on the **existing** per-aggregate port. A fourth
`internal/ports/outbound/` directory fails `packages_test.go:205-211`, and a
member arrives with the consumer that needs it.

- **Keyset, never `OFFSET`, on every table that grows.** The reason is already
  written down at `persistence/accounting.go:94-100`: "OFFSET re-reads and
  re-discards every row already passed... and a pass that pages by OFFSET while
  legs land concurrently can both skip a row and read one twice." For an
  append-only money table that is a reconciliation finding waiting to happen.
- **The ledger page is per-bucket.** `funding_buckets.last_sequence` is
  per-bucket and the existing unique index is `(funding_bucket_id, sequence)`,
  so `(bucket, sequence)` is a direct range scan with no new index. A
  cross-bucket timeline keyed on `created_at` would need an index that does not
  exist, built with plain `CREATE INDEX` on a table this lane's own rule makes
  non-`CONCURRENTLY` — a multi-hour exclusive lock as a deployment decision.
  The per-bucket page is the feature; the timeline is a later migration that
  names its own lock window.
- **No totals, on any list.** The console's first list contract carries over the
  cross-plane feed's three parts — `items`, `next_cursor`, `has_more` — with
  the reasons restated, and says "more available", never "of N". A page-number
  widget is not built: a keyset cursor names a position, and `?page=3` names
  no position in any order.
- **A cursor is meaningless under different filters**, and the cursor is
  opaque so the server cannot compare one. It therefore carries a filter
  fingerprint, and a mismatch is `invalid_request`. Without it, an operator
  pages bucket A, switches the dropdown to B, and reads A's money history
  under B's heading.
- **The three cached balances are rendered, never computed.** The database
  already checks `available = settled - held`; a frontend that summed legs
  would be displaying its own arithmetic as if it were data, and would need
  every leg of a bucket to do it.
- **The currency is a contract decision, not a rendering decision.** Every money
  response carries the settlement currency and its minor-unit exponent, and
  the console renders what the API declares. Issue
  [#63](https://github.com/ecoma-io/llm-gateway/issues/63) records the same gap
  on the fact feed; a console that renders bare minor units as dollars is the
  same defect one plane over.

### 5. What the first console does not have

Named, so that their absence is a decision a reviewer can see rather than a
gap they have to infer:

| Not in this change                             | Why                                                                                                                                                                                                                                               |
| ---------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Model catalog, request explorer, runtime views | Data Plane state with no contract on `dataplane.yaml` and no index on `public.requests`. Each is a change to the Data Plane, a contract addition and a migration before it is a page.                                                             |
| Spend-over-time charts                         | Loom ships no charting primitive, and the number would be a client-side aggregate — the one thing the brief's own rule forbids.                                                                                                                   |
| Repair, refund, or adjustment from a finding   | `ReconciliationFindings` is not a repair path and cannot be made into one. Operator correction goes through `Accounting.Adjust`, which requires an operator identity, a reason and the original leg, and is a separate, separately-confirmed act. |
| "Roll now", "expire now", "promote now"        | The due-work lanes are worker-owned. A button that cannot do what its label promises is worse than no button.                                                                                                                                     |
| Coverage figures, staleness thresholds         | "a coverage figure that reads as coverage and measures nothing", and a staleness threshold "cannot be made from this plane's own rows".                                                                                                           |
| An account switcher, multi-account views       | A user has exactly one account. Seeing two is being two users, which is a sign-out, not a scope.                                                                                                                                                  |

### 6. The failure matrix is a gate, not a document

Every code in `shared/errors.yaml` gets a declared console behaviour before
this change lands, and the generated `Error["code"]` union gets a test that
fails when a new member appears with no behaviour. The generated union comes
from the contract, so adding a code breaks a test until someone writes the UI
for it — which is rule 6 satisfied mechanically rather than by review.

Three codes are unreachable from the console by construction —
`unsupported_version`, `revision_gap` and `snapshot_required` are projection
codes with no console operation — and that is asserted too, so the plane
boundary is pinned from the UI side.

`upstream_unavailable` and `internal` are rendered differently and tested as a
pair: one offers a retry and keeps the last good data visible, the other offers
none and shows a `request_id` the operator can quote. `internal` never carries
the server's cause; `writeError` already logs the request id and nothing else,
and every new handler inherits that.

### 7. Contrast, contrast, and the frontend boundary

- **`--color-input` is 1.42:1 on `--color-background` in light and 1.88:1 in
  dark**, computed from `loom`'s own token values. It is the border of
  `TextField` and `Checkbox`, and WCAG 1.4.11 puts a 3:1 floor on the visual
  information that identifies a control. Filed as
  [`ecoma-io/loom#475`](https://github.com/ecoma-io/loom/issues/475) rather than
  patched locally, because a design-system token defect belongs to the design
  system. Until it lands, every form control the console renders carries a
  non-colour affordance, and an invalid field uses `border-destructive` (6.25:1)
  rather than relying on the base border.
- **`--color-muted-foreground-soft` is graphical-only** and is not used for
  text anywhere in the console.
- **The frontend gets the boundary rule the Go side already has.** No import
  rule, no layer linter and no `archkeep-attestation.json` exist for
  `apps/console` today; `check-projects.mjs` is a build-graph gate, not a
  boundary gate. `internal/arch`'s `TestTheRuleRosterIsTheDeclaredOne` is the
  template — a pinned roster, so a rule that stops governing anything fails.
- **`console.yaml` is not split.** Growth from three operations to forty is a
  size, not a correctness problem; the gates that matter are per-document.
  Product schemas move to `shared/` fragments referenced by `$ref`, which is
  the pattern the document already under-uses. A fourth document would break
  `package.json`'s three-path Spectral invocation and
  `openapi-ts.config.ts`'s single `input:`, and would contradict rule 2.
- **Loom's `Pagination` is not used for list pages.** It requires a `total`
  and renders `<button>`s, and its own docblock says a product whose pages are
  real URLs "wants its own `<a href>` row instead of this component". The
  console's pagination is a `<nav>` of `RouterLink`s with `aria-current`, so
  Back undoes a page change.
- **Status is never colour-only.** One presentation map per domain, keyed
  exhaustively over the generated enum with `satisfies Record<Enum, …>`, so
  the contract growing breaks compilation. Colour, an `aria-hidden` icon, and
  a human label — three channels, so it survives greyscale. The badge on
  `GatewayStatusPage.vue:87` renders the raw contract token `ok` as its text
  today; that stops.
- **Focus is never lost to `<body>`.** A re-render that unmounts the focused
  element dumps a screen-reader user at document start. Every list ships the
  return-focus discipline Loom's `DataGrid` already implements internally
  (`:280-300`), and the sticky `AppHeader` gets measured `scroll-padding-top`
  so a focused control is not obscured behind it — WCAG 2.2 SC 2.4.11, for
  which no axe rule exists at all.
- **Automated accessibility is claimed only for what can be checked.** Loom
  already exports `BROWSERLESS_RULES` from `@ecoma-io/loom/a11y` with a
  measured jsdom-versus-browser argument per rule, and that array is the jsdom
  gate: no new dependency for the rule list, `axe-core` for the engine. The
  seventeen browser-required rules — `color-contrast`, `target-size` among them —
  are stated in the pull request as verified manually, and the pull request
  does not claim WCAG 2.2 AA on the strength of a green jsdom run.

### 8. Scope of this change

One change, and it is the smaller half of the proposal:

1. The session surface — sign-in, session, sign-out, the principal, the
   authorization predicate at the query, the origin and content-type guards,
   `control.sessions` in migration `000010`, and the errors `unauthenticated`
   and `invalid_request` given code paths.
2. The Control-Plane read surface: accounts, users, API keys (with the one-time
   mint), plans, subscriptions, entitlements, funding buckets with their three
   balances, per-bucket ledger, findings and runs.
3. The console built on Loom primitives, with the state, focus, status and
   contrast rules above, server-side pagination with URL state, and a
   first-party `<DataTable>` that renders `<caption>`, `<thead>` and a
   `<th scope="row">` — Loom's `Table` supplies a bare `<table>` slot, and
   `TableCell` renders `<td>` and never `<th>`, so the first column of every
   list would otherwise have no row header at all.

The Data-Plane half of the proposal is filed as its own work, ahead of its
screens rather than alongside them.

## Consequences

- **The Control Plane gains a third table and an authentication surface**, and
  the security rules this product surface asserts stop being aspirations. The
  existing `verify.sh` control-table assertion is an exact `string_agg`
  equality, so `sessions` extends that list in the same change — the equality
  stays an equality, because relaxing it to a containment check would silently
  unconstrain the twenty-one tables already there.
- **`contract_test.go` needs a test-local constructor**, and that is the exact
  moment the panic-on-nil-port discipline gets quietly defeated if nobody says
  so. It is named here so the test is written that way from the start.
- **Every money value on a screen is a decision about currency** that the
  contract has to answer first. A number that renders without a unit is worse
  than a number that refuses to render.
- **Two ADRs are amended rather than contradicted.** ADR 0006 §3 gains nothing
  — it already said console orchestration — but §6 gains the statement that the
  console's cross-domain joins are use cases, and §9's asymmetry is restated so
  the façade word is not read onto `console-api` again. ADR 0008 is amended for
  the `invited` credential decision this record makes.
- **The three deferred features are deferred honestly.** Each needs a
  `dataplane.yaml` operation and, for the request explorer, a migration that
  indexes `public.requests` on `(account_id, admitted_at)` and
  `(admitted_at, id)` — in the same migration as the read that needs it, as
  `000008_intake_request_lookup` did.
- **A console that renders a Data Plane outage and a bug identically is the
  defect this change is most likely to ship**, and the test that catches it is
  the pair in §6. It is written before the pages, not after.

## Alternatives considered

- **Ship the screens first, add sign-in after**: rejected. It is the ordering
  that mints live credentials on an unauthenticated endpoint, and it is the
  only ordering in which no test in this repository is red at any point.
- **A token in `sessionStorage` instead of a cookie**: rejected. The XSS case
  is identical, and the brief's own rule forbids it. `httpOnly` is the only
  option in which the browser cannot read the session at all.
- **Path-scoped `/accounts/{account_id}/...`**: rejected. It puts every
  operator's read of every customer's ledger into every reverse proxy's access
  log, and a `Referer` header. The account comes from the session.
- **`403` for a resource belonging to another account**: rejected. It confirms
  that a guessed id is real. The account predicate moves into the query, which
  returns zero rows for both cases at the same cost.
- **A three-role RBAC model**: rejected for this change. It is a role model
  nobody reviewed, and changing it later is a data migration over live
  authorization decisions. Two classes, read from the session, is the 90% that
  actually prevents the cross-tenant read.
- **One endpoint per screen, composed in the page component**: rejected. Six
  tiles over six calls is six pool connections for one operator's screen, and
  any real cross-domain join becomes arithmetic in the browser — the rule the
  proposal itself set.
- **A cross-bucket ledger timeline sorted by time**: rejected here, named
  above. It is a separate migration that has to name its index and its
  `CREATE INDEX` lock window on a table that reaches 584M rows a year.
- **Letting the frontend sum ledger legs for a page total**: rejected twice
  over. The page is not the ledger, and a `Σ amount` across `grant`, `topup`,
  `hold`, `release`, `consume` and `adjustment` is not a number any of those
  kinds means.
