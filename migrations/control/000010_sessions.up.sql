-- Migrations lane: control — the Control Plane's `control` database
-- (ADR 0006 §7). This file lands the console's authentication surface: the
-- browser session itself, and the credential the console user proves who
-- they are with. ADR 0012 §2 makes this the first change in the console's
-- history for the reason that no product endpoint may arrive before it — an
-- unauthenticated `POST /accounts/{id}/api-keys` mints a live credential and
-- nothing in this repository would notice.
--
-- 000010 — the session surface (ADR 0012 §2, §3; issue #41):
--
--   Two objects, and they belong in one file rather than two. `sessions` is
--   new, but the credential is three columns on `users`, and splitting them
--   would mean a database in which a user has a session and no way to have
--   proved who they were — a state the sign-in path must defend and no reader
--   of the schema could see. Rule 7 numbers a file per lane, not per table,
--   and the lane's own `users` precedent (000005 tightening a table 000002
--   created) is a file whose subject is a table an earlier file built.
--
--   What this file does NOT build, deliberately: the operator predicate. The
--   `operator` class is admitted in the vocabulary and minted by nothing, and
--   that is the point — see the class section below.
--
-- ---------------------------------------------------------------------------
-- sessions — one browser session, opaque, server-side, stored hashed.
-- ---------------------------------------------------------------------------
--   The token is 256 random bits rendered `ses_<base64url>`, handed to the
--   browser once in a `HttpOnly; Secure; SameSite=Strict; Path=/` cookie, and
--   never persisted. Opaque rather than signed, so sign-out is an immediate
--   revocation rather than a token that stays valid until it expires, and so
--   nothing in this table has to be re-signed when the cookie's claims change.
--
--   `token_hash` is the SHA-256 hex digest of that token, and it is the
--   lookup every authenticated request performs. The unique constraint below
--   IS the index that serves it, and that is the reason the constraint is not
--   negotiable: a lookup that scanned would answer "is this cookie valid" in
--   time proportional to how many sessions exist, which is a timing oracle on
--   the token itself and defeats the entire opaque-session design. The
--   digest is computed, not searched, so a row that stored the token instead
--   would be a usable cookie sitting in the table — and the shape check
--   refuses exactly that row, which is the one mistake this column invites.
--   The shape check is therefore not cosmetic: it is what makes "we store the
--   digest, never the token" a statement the database enforces rather than a
--   property of a code path somebody has to remember.
--
--   `class` is the HOLDER'S AUTHORISATION CLASS, and it is deliberately not
--   `identity.Kind`. The two are orthogonal axes and conflating them fails in
--   both directions:
--
--     * `identity.Kind` says WHICH CREDENTIAL AUTHENTICATED —
--       PrincipalUser for a console identity, PrincipalAPIKey for a runtime
--       key. A session is a browser credential, so its Kind is always
--       PrincipalUser, on every row, forever. A column that could say
--       otherwise would be describing a credential kind the runtime has no
--       way to present.
--     * `class` says WHAT THAT HOLDER MAY DO ACROSS ACCOUNTS — a `user` acts
--       for exactly the one account ADR 0001 rule 2 gives them, an `operator`
--       administers several.
--
--   So the column lives on the session's own aggregate rather than on
--   `Principal` (ADR 0012 §2, and the amendment that record carries), and it
--   is set at creation from the server's own decision and never read from a
--   request. `user` is the ONLY class this change mints. `operator` is in the
--   vocabulary, and that admission is the whole reason for it: a staff
--   surface later is then a new predicate over rows that already hold a
--   class, rather than a migration that re-derives a column's meaning across
--   live authorization decisions. The predicate is not written, so an
--   operator session is UNREACHABLE rather than merely unused — and the test
--   worth having there is that no signed path produces one, which is a
--   handler's test and not this schema's.
--
--   `operator_id` is deliberately absent as a column, and the contract types
--   it as text for the reason `domain/accounting/ids.go:55-58` gives: the
--   operator surface is not built and the grammar of a staff identity is not
--   something this schema guesses at. A text column nobody writes would be a
--   promise about a vocabulary nobody has agreed on, so the day it is needed
--   it arrives as a column with a CHECK behind it, on a change that also
--   brings the predicate.
--
--   The `user_id` edge is COMPOSITE — (user_id, account_id) into users
--   (id, account_id), over the unique 000005 already established — and that
--   is the same argument 000005 makes for the API key's creator, unchanged: a
--   single-column reference proves the user exists SOMEWHERE, and a user id
--   alone cannot pin the account, because user ids are unique across the
--   table and one from any account satisfies the reference exactly as well.
--   A session whose account and user disagree is precisely the leak ADR 0012
--   §2 refuses to build by path — a principal reading another account's
--   surface — and the account a session acts for is the one column every
--   product operation authorizes against. The composite is also the shape
--   that survives the operator session: when `operator` is finally minted
--   and `user_id` goes nullable, MATCH SIMPLE passes NULL exactly as the
--   nullable API-key creator does today, so admitting the staff class later
--   is a relaxation of this constraint rather than a rewrite of it.
--
--   EXPIRY IS NOT A STATE. `state` is `active | revoked` — the api_keys
--   vocabulary, with the same `(state = 'revoked') = (revoked_at IS NOT NULL)`
--   pairing, because status and its instant are the same sentence told twice
--   and a row where they disagree is a writer that crashed between the two
--   writes. An `expired` third state is deliberately absent: expiry is a
--   FACT (`expires_at`), and whether the fact has come to pass is read
--   against a clock at lookup time. Recording it as a column would make a
--   second source of truth for one fact, and whichever the sweeper missed
--   would be a session that reads live past its own expiry. This is the same
--   decision 000009 made for the high-water mark — persist the fact, never
--   recompute the bound — and an expired row is refused by the read
--   predicate whether or not any sweeper has ever run.
--
--   No DELETE path and no CASCADE, per the lane: a revoked session is a
--   state, and the row is the evidence that a principal was signed in.
--
-- ---------------------------------------------------------------------------
-- users — the console credential, and why its parameters are per-row.
-- ---------------------------------------------------------------------------
--   ADR 0012 §2 fixes the primitive: PBKDF2-HMAC-SHA256 from `crypto/pbkdf2`,
--   which is standard library in Go 1.26 (`go doc crypto/pbkdf2` resolves in
--   this repository's toolchain). No new dependency enters the module, which
--   is AGENTS.md rule 11 satisfied by not needing it: a password KDF is not
--   a place to introduce a transitive surface, and the API-key path already
--   established the lane's rule — a DIGEST persists, not a secret.
--
--   All three columns are NULLABLE, and that is not a gap. An `invited`
--   identity is a live row with no credential behind it (ADR 0012 §2: an
--   invitation is record-keeping, a credential is a grant of access, and
--   conflating them makes a leaked invitation email access), so "no
--   credential yet" is a state this table must be able to hold and not
--   merely a value that has not been filled in. What is refused is a HALF
--   credential: the presence check below requires all three columns or none,
--   because a row holding a digest with no iteration count is a credential
--   nobody can ever verify, and one nobody can verify is indistinguishable
--   from a wrong password only by accident.
--
--   A CHECK tying `state` to credential presence is deliberately ABSENT, and
--   the omission is a decision rather than an oversight. ADR 0012 §2 decides
--   that an invited identity MAY NOT EXERCISE a credential; that is an
--   admission rule — "may this holder prove who they are at all" — and it
--   belongs in the sign-in use case, where the other two of its three inputs
--   (email, account state) are already answered. Pinning presence to
--   `state` here would restate an application rule in the schema, and would
--   make every future answer to "what may an invited user do" a migration
--   over a live column. The schema's job is narrower and it does it fully:
--   the credential is well-formed, whole, and non-guessable, and the
--   revocation/absence halves are legible.
--
--   The ITERATION COUNT IS A COLUMN, not a constant, and this is the one
--   place the lane has no precedent to defer to, so the reasoning is
--   recorded. The count is the parameter that must RISE: PBKDF2 costs grow
--   with hardware, and a stored count lets a later migration raise it and
--   have every existing row still verify under its own recorded cost,
--   re-deriving each on its next successful sign-in. A constant would make
--   the raise a data migration over every live password at once — either
--   forcing a reset nobody asked for or breaking sign-in outright. The PRF
--   and the derived-key length are NOT columns, because they are not going to
--   change: they are the fixed grammar of the stored digest, and the shape
--   checks below are what pins them. A migration that changes the primitive
--   changes the column names, which is a change a reviewer can see.
--
--   The salt is per-row and 16 bytes, not a global pepper: a global pepper
--   would be a second secret the database needs and a compromise would then
--   expose every credential at once, which is the same single point of
--   failure the lane refuses in the API-key record.
--
-- ---------------------------------------------------------------------------
-- The lookup indexes, and the one query that has none.
-- ---------------------------------------------------------------------------
--   `token_hash`'s unique constraint is the sign-in access path (above), so
--   there is deliberately no second index beside it: 000009's own argument
--   against a redundant index applies verbatim, and a second b-tree over the
--   same column would be paid for on every sign-in and read by no plan.
--
--   `sessions_live_expiry_idx` is the OTHER question, and it is a question
--   this table does not yet ask in application code: which LIVE sessions are
--   past their expiry, for a periodic sweep to retire. It is a partial index
--   over `revoked_at IS NULL` because that is the exact shape of the answer —
--   a revoked row is already dead and its `expires_at` decides nothing, so
--   the predicate keeps the accumulating history out of the index and leaves
--   a sweep's walk proportional to the LIVE set rather than to everything the
--   table has ever held.
--
--   It is worth being explicit that CORRECTNESS DOES NOT DEPEND ON THIS
--   INDEX OR ON A SWEEPER EXISTING. An expired session is refused by the
--   read predicate against `expires_at`; delete nothing and expiry still
--   works. What the index serves is GROWTH — a table of sessions that is
--   only ever appended to and never pruned is a table whose answer to "what
--   is live" gets slower for the life of the deployment. It is added now
--   because adding it later means a migration that is concurrent-only and
--   therefore the one shape the lane's transaction rule forbids, which is
--   this repository's standing argument for deciding indexes at the table's
--   birth rather than under pressure.
--
-- ---------------------------------------------------------------------------
-- Lane conventions, unchanged and re-stated where this file touches them.
-- ---------------------------------------------------------------------------
--   * explicit names on every constraint and index, in the lane's
--     default-derived vocabulary, so a future migration can name what it
--     alters and an error message names what fired;
--   * timestamptz everywhere, application-supplied, NOT NULL — the lane trusts
--     the writing transaction's clock more than any other;
--   * uuid v7 on ids this plane mints, shape-checked, matching the identity
--     and accounting foundations. The v7 CHECK is present on the columns
--     000003 and 000006 introduced and absent from the three 000002 shipped
--     before the convention existed; the ALTER below deliberately adds no v7
--     check to `users.id`, because a check is not a repair and tightening a
--     shipped table's primary key is 000005's subject rather than this
--     file's.
--   * no DELETE path and no ON DELETE CASCADE anywhere: both foreign keys
--     here are RESTRICT by default on purpose.
--
--   The file carries no BEGIN, COMMIT or ROLLBACK of its own, per
--   migrations/README.md: the runner delivers it as one simple query — one
--   implicit transaction — and the migration fails or applies whole. An
--   ALTER TABLE validates every existing row it touches when it applies, and
--   the credential columns are additive with nullable shapes, so this file
--   migrates no data it does not write.
--
-- The inverse of this file is 000010_sessions.down.sql, in reverse creation
-- order, no CASCADE.

-- ---------------------------------------------------------------------------
-- sessions — one browser session, opaque, server-side, stored hashed.
-- ---------------------------------------------------------------------------
CREATE TABLE control.sessions (
    id uuid PRIMARY KEY
        CONSTRAINT sessions_id_uuid_v7
        CHECK (id::text ~ '^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'),
    user_id uuid NOT NULL
        CONSTRAINT sessions_user_id_fkey
        REFERENCES control.users (id),
    account_id uuid NOT NULL
        CONSTRAINT sessions_account_id_fkey
        REFERENCES control.accounts (id),
    -- The SHA-256 hex digest of the session token, and never the token. See
    -- the header: the shape check is what makes "digest, not secret" an
    -- enforced fact rather than a promise, and the unique constraint is the
    -- index that keeps a cookie lookup from being a timing oracle.
    token_hash text NOT NULL
        CONSTRAINT sessions_token_hash_shape
        CHECK (token_hash ~ '^[0-9a-f]{64}$')
        CONSTRAINT sessions_token_hash_key UNIQUE,
    -- The holder's authorisation class, NOT identity.Kind — see the header for
    -- why the two are orthogonal and why `operator` is in the vocabulary with
    -- no predicate to mint it.
    class text NOT NULL
        CONSTRAINT sessions_class_valid
        CHECK (class IN ('user', 'operator')),
    state text NOT NULL
        CONSTRAINT sessions_state_valid
        CHECK (state IN ('active', 'revoked')),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    revoked_at timestamptz,
    -- The account a session acts for is the account its user belongs to. A
    -- single-column user reference cannot say that — 000005's argument,
    -- unchanged — and a session whose two accounts disagree is a principal
    -- reading someone else's surface. The composite references the unique
    -- (id, account_id) 000005 already put on `users`, so it adds no new key.
    CONSTRAINT sessions_user_id_account_id_fkey
        FOREIGN KEY (user_id, account_id)
        REFERENCES control.users (id, account_id),
    -- The api_keys pairing, on this table's own lifecycle: a revocation has
    -- an instant and a live session has none, and both directions are pinned
    -- because "revoked with no revoked_at" and "active with one" are both
    -- states a bug in the transition would produce.
    CONSTRAINT sessions_revocation_consistency
        CHECK ((state = 'revoked') = (revoked_at IS NOT NULL)),
    -- The three orderings, and nothing more. An expiry at or before creation
    -- is a session that granted nothing; a last-seen or a revocation before
    -- the row existed is a clock bug. What is NOT pinned is a last-seen past
    -- the expiry: whether a session may be served is the read predicate's
    -- answer against `expires_at`, and a guard here would be a second rule
    -- about the same fact that could refuse a legitimate write under clock
    -- skew between the two instants the application supplies.
    CONSTRAINT sessions_expiry_order
        CHECK (expires_at > created_at),
    CONSTRAINT sessions_last_seen_order
        CHECK (last_seen_at >= created_at),
    CONSTRAINT sessions_revocation_order
        CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

-- The sign-in access path is `token_hash`'s unique constraint above; this is
-- the other question, and it is the sweep's. Partial over revoked rows
-- because a revoked session is already dead and its expiry decides nothing —
-- the predicate keeps the accumulating history out of the index, so a sweep's
-- walk stays proportional to the live set. Nothing in the application asks
-- this yet and correctness does not depend on it: an expired session is
-- refused by the read predicate whether or not a sweeper ever runs. See the
-- header for why it lands now rather than under growth pressure.
CREATE INDEX sessions_live_expiry_idx
    ON control.sessions (expires_at)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE control.sessions IS
    'A browser session (ADR 0012 §2): an opaque 256-bit token delivered once in an HttpOnly; Secure; SameSite=Strict cookie, of which only the SHA-256 digest is stored. Opaque rather than signed so sign-out is an immediate revocation. Expiry is a fact in expires_at, not a state — an expired session is refused by the read predicate, with or without a sweeper.';
COMMENT ON COLUMN control.sessions.token_hash IS
    'Lowercase hex SHA-256 digest of the session token, and never the token. The unique constraint is the sign-in access path: a lookup that scanned would time-proportion its answer to how many sessions exist, which is an oracle on the token itself. The shape check refuses a row that stored the token.';
COMMENT ON COLUMN control.sessions.class IS
    'The HOLDER''S authorisation class: user acts for exactly its one account, operator administers several. Deliberately NOT identity.Kind, which is the orthogonal axis saying which credential authenticated — a session is a browser credential, so its Kind is always PrincipalUser. user is the only class this change mints; operator is in the vocabulary with no predicate, so the staff surface later is a new predicate rather than a migration over live authorization decisions.';
COMMENT ON COLUMN control.sessions.state IS
    'active -> revoked. Revoked is terminal; there is no un-revoke. Expiry is not a state: an expired session reads live in this column and is refused against expires_at, so the two facts cannot disagree.';
COMMENT ON COLUMN control.sessions.revoked_at IS
    'Revocation instant; non-null exactly when state = revoked (sessions_revocation_consistency). A revocation is immediate and total — the row survives as evidence that the principal was signed in.';
COMMENT ON COLUMN control.sessions.last_seen_at IS
    'Last instant this session authenticated a request. A column, not identity: moving it must never be what makes a second row, and a sweeper ordered by it would be ordering by a value every request rewrites.';

-- ---------------------------------------------------------------------------
-- users — the console credential (ADR 0012 §2).
-- ---------------------------------------------------------------------------
-- PBKDF2-HMAC-SHA256 from the Go 1.26 standard library, so no new dependency
-- enters the module (AGENTS.md rule 11). All three are nullable because an
-- `invited` identity legitimately has no credential; what is refused is a
-- HALF credential, and the check at the end is the reason: a digest with no
-- iteration count is a credential nobody can ever verify.
--
-- The iteration count is a column rather than a constant because it is the
-- parameter that must rise over the life of the deployment, and a per-row
-- value lets a later migration raise it with every existing row still
-- verifying under its own recorded cost. A constant would make the raise a
-- data migration over every live password at once. The PRF and the key
-- length are NOT columns: they are the fixed grammar of the digest, and
-- changing the primitive changes the columns, which is a visible change.
ALTER TABLE control.users
    ADD COLUMN credential_hash text
        CONSTRAINT users_credential_hash_shape
        CHECK (credential_hash IS NULL OR credential_hash ~ '^[0-9a-f]{64}$'),
    ADD COLUMN credential_salt text
        CONSTRAINT users_credential_salt_shape
        CHECK (credential_salt IS NULL OR credential_salt ~ '^[0-9a-f]{32}$'),
    ADD COLUMN credential_iterations integer
        CONSTRAINT users_credential_iterations_positive
        CHECK (credential_iterations IS NULL OR credential_iterations > 0),
    -- Whole or absent, all three together. A half-written credential is
    -- either an unusable row or a silently weaker one, and both are bugs a
    -- later sign-in would report as a wrong password. Table-level, not a
    -- column constraint, because the rule is about the three columns in
    -- relation and no single column can see the other two.
    ADD CONSTRAINT users_credential_whole
        CHECK (
            (credential_hash IS NULL) = (credential_salt IS NULL)
            AND (credential_hash IS NULL) = (credential_iterations IS NULL)
        );

COMMENT ON COLUMN control.users.credential_hash IS
    'Lowercase hex PBKDF2-HMAC-SHA256 derived key (crypto/pbkdf2, Go 1.26 standard library — no new dependency), verified under this row''s own iteration count. NULL while the identity is invited: an invitation is record-keeping and a credential is a grant of access (ADR 0012 §2). The state/credential pairing is deliberately not a CHECK here — that rule is the sign-in use case''s admission predicate, not the schema''s.';
COMMENT ON COLUMN control.users.credential_salt IS
    'Per-row 16-byte salt, lowercase hex, generated per credential. Not a global pepper: a pepper is a second secret the database must hold, and its compromise would expose every credential at once.';
COMMENT ON COLUMN control.users.credential_iterations IS
    'PBKDF2 iteration count this credential was derived under, stored per row so a future raise re-derives each credential on its next successful sign-in instead of invalidating every live password at once. A constant would make that raise a migration over live credentials.';
