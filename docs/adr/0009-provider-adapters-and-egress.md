# ADR 0009: Provider adapters and egress — the executor boundary, the settled claim, and the renewed lease

- Status: Accepted
- Date: 2026-09-26
- Issue: [#92](https://github.com/ecoma-io/llm-gateway/issues/92)

## Context

ADR 0002 designed the routing model around a layer that did not exist: the
candidate **execution** layer — the adapters that translate a request for a
provider's protocol and call it. Its pipeline, disposition tables and
commitment gate were written so that the missing layer could be added without
changing them, and for a while the runtime proved the negative case: the
registry the routing stage read was empty, and every admitted request was
released as a no-candidate answer. The walk, its dispositions and its ending
transactions were landed and tested; what no commit had delivered was the
thing they were waiting for.

Landing it raised the questions a design names but does not answer until the
code has to:

- **What does one adapter trust, and what does it decide?** The boundary had a
  who (ADR 0002's layer ownership) but no contract for the call itself: what
  an adapter may re-issue, where the answer's commitment is crossed, what it
  does when its credential cannot be resolved.
- **What does the gateway claim when the provider's report and the hold
  disagree?** A report above the count the hold was priced from, or no report
  at all, had no recorded rule; the first adapter would have invented one.
- **What keeps a live call's hold alive?** ADR 0004 says "the live executor
  renews a reservation's execution lease" without saying who runs the
  renewal, what clock it follows, or what each of its three possible endings
  means for a call in flight.
- **What happens when the hold is gone anyway?** A renewal that loses its
  compare-and-set, a walk whose caller vanished mid-stream: the vocabulary
  had the `gateway_abandoned` word and no serving path that could use it.
- **Which duration bound actually protects a call from the reaper?** The
  design pages said "the maximum accepted request/stream duration is bounded
  strictly below the maximum lease". That names the wrong pole — see
  [The enforceable bound](#the-enforceable-bound-is-the-hold-window).

The cost of not deciding now was the first adapter deciding each of these
silently, in the one code path every later adapter would copy.

## Decision

### The executor port: one call, typed results, the sink owns commitment

An executor is one frozen wiring — endpoint, credential closure, egress dial —
behind one method. `Execute` takes the admitted body, the attempt's identity
and the reply's sink, performs **exactly one** upstream call, and returns one
typed result: a success carrying the provider's usage report, or a failure
classified into ADR 0002's closed vocabulary. The rules, restated as the
port's contract:

- **An adapter re-issues nothing.** Retry-shaped behavior — next candidate,
  next egress route, second attempt on a stalled stream — belongs to the
  layers that own it. A 3xx is refused where it stands, not followed.
- **The sink is the commitment point.** An adapter writes only
  content-bearing bytes into the sink, and the first `Content` call the
  reply accepts is commitment (the one-way gate, ADR 0002). The preamble —
  the lifecycle frames a stream answers with before its first content — is
  read bounded and held by the adapter, then flushed into the sink ahead of
  the first content; if the preamble reaches its cap, the flush itself is
  the commitment, because bytes a caller can already read are content
  whatever named them. Lifecycle frames never reach the sink on their own.
- **No I/O beside the one call.** No catalog reads, no configuration reads,
  no store access inside `Execute`. Everything an adapter needs arrives
  frozen at construction — which is what makes a snapshot of them safe to
  serve from.
- **Credentials travel as a closure.** The adapter holds a resolver, not
  material: it asks once per call, and an answer of "absent" ends the call as
  `authentication` before anything leaves the process — fail-closed, because
  a backend wired without material must neither vanish from the walk's
  answers nor send an unauthenticated call.
- **Redaction is the adapter's habit.** A non-2xx envelope is telemetry: read
  bounded, stripped of auth-shaped keys and credential-shaped values, capped
  at a bound the vocabulary exports — the same number on the writer and the
  wall.

The first adapter behind the port speaks the OpenAI chat-completions wire
(`adapter_type = 'openai-compatible'`): the admitted body carried over, the
candidate's provider model pinned over whatever the caller named, the
operator's parameter overrides on top, and two fields the gateway owns — the
stream switch and, for a streamed call, `stream_options.include_usage`, which
the provider is never allowed to opt away because the settlement reads it.
The usage report is required, not hoped for.

### The registry is a snapshot, and the composition root builds it

The law's subject is the **executor**: an adapter must never read the
catalog, because a provider call that opens a database read would hold a
connection across a call that can outlive minutes. The routing stage still
reads the catalog, on purpose — the walk's eligibility leg is a per-request
read of the alias's candidates and their backends, each read its own short
unit that closes before any call runs. What the snapshot buys is that the
call itself needs no catalog: the registry is one frozen executor per
callable backend row, built by the composition root from one whole read of
the catalog, swapped whole behind an atomic pointer. Its laws:

- The walk's lookup is a map read: I/O-free, identical between refreshes, so
  an attempt row's meaning never shifts under a request in flight.
- Only rows whose adapter type this build speaks get executors. A row of an
  unknown type is real catalog data with no driver yet — the set is open by
  design, and its candidates resolve as unavailable, exactly the shape of a
  backend that does not exist.
- A row that cannot be resolved — an endpoint the adapter refuses, a
  credential grammar nothing speaks, an egress policy the configuration does
  not define — is **left out of the snapshot and named in the log**, never
  guessed into callability and never a crash.
- A refresh whose read fails changes nothing: the last good snapshot keeps
  serving, because an empty registry would turn one bad read into every
  answer being no_candidate.
- A refresh whose rows read the same as the last build swaps nothing:
  executors are frozen, and rebuilding them would retire transports that may
  hold live streams.

Disabling a backend remains eligibility's business — the catalog's `state`
filters at candidate selection, not at snapshot build — so an operator's
disable/enable cycle is a catalog fact the walk honours, not a rebuild the
registry must notice.

### The settle basis: settled ≤ hold, by construction

The gateway claims, for every settled ending, figures bounded by the counts
the hold was derived from:

- **Input** is the provider's report bounded by the input count admission
  priced the hold from — a report above it clamps down to it, and no report
  falls back to it.
- **Output** is the provider's report bounded by the output basis the hold
  was sized with (the request's ceiling, itself bounded by the alias), and
  the basis itself when no report arrived — never the delivered byte count,
  which would price a transport estimate the provider's tokenizer never
  agreed to.
- **Delivery** is recorded beside the settlement as the gateway's own count
  of what left the process, and prices nothing — ADR 0003 prices input and
  output only, and a byte-count heuristic is not a tokenizer (B11 replaces
  the interim count).
- **The capture label attests to provenance and nothing else**: `reported`
  when the provider's own figures stand as given, `reservation_floor`
  whenever a settled figure is the reservation's own — a clamp that bound, or
  an output nobody reported.

With both figures bounded by the hold's own counts, the settled amount cannot
exceed the hold — the hold formula is increasing in both — so the first law
of the ending, settled ≤ hold, holds by construction rather than by check.
The provider's own claims are not lost by the clamping: the attempt row
records them as the telemetry they are, and the fact prices only what the
reservation defends.

### The lease renewer rides the walk, not the adapter

"The live executor renews" was the design's shorthand; the code's answer is
that the **routing stage's walk** runs the renewal, around the executor call,
because the renewal is the hold's business and no adapter's. One renewer per
call:

- The **deadline chain is monotone and anchored at admission**: the first
  renewal extends the admission-stamped expiry by the lease TTL, and every
  later renewal extends the previous deadline by the TTL. The chain belongs
  to the **walk**, not to one call — a candidate that fails after burning
  several lease lifetimes leaves the chain where its renewer left it, and
  the next candidate's renewer continues from there instead of restarting
  from the admission stamp, which a long first call has already left
  behind. No node clock enters the arithmetic, so skew never shortens what
  the previous renewal bought, and a deadline never shrinks.
- The **first renewal is immediate** — the walk starts after admission's
  stamp, and part of the TTL may already be spent — then ticks at a third of
  the TTL: two renewal chances inside one lease lifetime.
- **A renewal answered false is the hold reporting itself gone** — the lease
  expired under the call and the reaper won the race. The renewer cancels the
  call's context. Before commitment, that ending is the abandoned word's: the
  hold is released through the domain's abandoned door, the outcome kind is
  abandoned, and no disposition applies to an answer that was never
  committed. After commitment, the ordinary settle runs — its compare-and-set
  loses the same race and records the orphan tail. A renewal answered with an
  **error is no verdict at all**: the claim stands, the call runs on, and the
  next tick asks again — an outage shorter than the remaining hold window
  must not end a call it did not have to.
- The renewer runs detached from the call's context and is stopped when the
  call returns; the stop reports whether the renewal ended the call, which is
  how the walk knows whether its ending is its own.

The renewal extends the **lease** only. It never extends the hold window —
`expires_at` is fixed at admission — which is exactly why the next section's
bound is enforceable.

### The abandoned word travels through the abandoned door

The planning contract for this work said `gateway_abandoned` would be
recorded through `FailBeforeCommitment`. The landed domain refuses that:
`FailBeforeCommitment` accepts only the surfaced refusal family —
`provider_rejected_request`, `context_too_large`,
`upstream_authentication` — because a request row failed _before commitment_
is one whose disposition the walk surfaced to a caller, while **an abandoned
row is the reaper's decision**, recorded through the request's own abandoned
door (`FailAbandoned`), with no attempt named and no disposition implied.
The serving path now uses that door when a renewal loses the hold
pre-commitment, and when a walk finds its caller's context dead — both are
the same fact, the hold is gone from the open set, and neither is a refusal
anyone surfaced. The intake side needed no change: its failure vocabulary
already knew the word.

The composition stays ADR 0006's: one unit of work releases the hold, in
the same shape every other release uses. One precision the intake's
vocabulary already knew: the `gateway_abandoned` **word** travels on the
request row's failure reason and the intake record — the fact feed carries
a plain `released` usage fact, because a fact prices nothing and names no
failure; the ending's word is the request's, not the feed's.

### The enforceable bound is the hold window

The protection a live call has against the reaper is not "duration below the
maximum lease". It is this chain, validated at process start:

- the **execution duration** — one provider call's context budget — must sit
  strictly below the **reservation hold window** (the horizon the reaper
  sweeps against);
- the lease TTL already sits strictly inside the hold window, and the
  renewer keeps the lease alive for as long as the call runs;
- so a call that obeys its budget never meets a reaper sweep that could take
  its hold: the sweep needs both a dead lease and a passed hold window, and
  the call's own renewal keeps the lease alive for every sweep inside its
  window.

A configuration that violates the chain is one the process refuses to start
with. Renewal cannot save a call allowed to outlive its hold — renewal keeps
the lease alive, never the window — which is why the bound is on the window,
and why ADR 0004's sentence "expires_at is strictly longer than the maximum
accepted request duration" is hereby amended: `expires_at` is the hold
window's stamp, the bound is the one above, and it is enforced at start, not
hoped for at runtime.

### Credentials are environment references; egress is a named policy

The catalog stores **references**, never material: a backend row's
`credentials_ref` is an opaque token the composition root resolves — this
build speaks the one grammar `env:NAME`, resolved at use so a rotated
variable is picked up by the next call rather than the next restart — and its
`egress_policy_ref` names a policy in the process configuration, whose
ordered routes become the egress layer's preference order. The empty
reference and the reserved word `direct` are the plain dialer. Where
credentials live is this decision's to change (issue #49's opacity is
preserved); that they never sit in the catalog was never open.

## Consequences

- The walk tries candidates. The no-candidate release remains exactly what
  it was for the aliases no candidate serves, and becomes the exhausted
  walk's answer for the aliases one does.
- A new provider is a row plus, if its wire is new, an adapter — the adapter
  roster is the one file that learns an `adapter_type` string, and the
  catalog's grammar already accepts the name.
- The empty-registry runtime is gone, and with it the certainty that every
  admitted request releases: the integration suite now serves a real
  provider over the real wire and settles its report.
- The reaper is still not wired — the sweep's driver remains open work
  (issue #80). Every law above is written so that wiring it adds a driver to
  an existing sweep, not a new semantics; the renewal-false handling exists
  and is tested against a fake reaper's verdict today.
- The interim input-token count (bytes, not tokens) remains the hold's basis
  until B11's tokenizer; the settle basis is written so the replacement
  changes the count's derivation and nothing else.
- The replay edge for delivered-content originals (probe answers 500, issue
  #90) is unchanged by this decision.

## Alternatives considered

- **Per-request executor construction** (build the adapter per call from a
  catalog read): rejected — it puts database I/O inside the call path the
  snapshot exists to keep I/O-free, and it makes an attempt's meaning depend
  on when the read happened.
- **Executor-side renewal** (the adapter renews its own lease around the
  call): rejected — the hold is the walk's subject, the renewal must survive
  the call's cancellation to release it honestly, and no adapter should know
  a lease exists.
- **Clamp the settled figures silently, no capture label**: rejected — the
  label is what keeps the clamp honest; a fact that cannot say whether its
  figures are the provider's words or the reservation's floor is a fact an
  operator cannot reconcile against a provider invoice.
- **Cancel the call on a renewal error as well as a renewal false**:
  rejected — an error is no verdict; cancelling on one converts a transient
  renewal-path failure into an abandoned ending the request did not earn.
  The both-clocks rule already bounds the exposure: an outage longer than
  the remaining hold window ends the call through the hold's own expiry.
