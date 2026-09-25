-- Migrations lane: dataplane — the Data Plane runtime's `dataplane` database
-- (ADR 0006 §7). This file is the lane's first schema repair: it widens the
-- terminal failure vocabulary a request and its replay record may carry. It
-- adds no table, no column and no index — two CHECK lists grow by three
-- values each, and that is the whole change.
--
-- 000007 — surfaced upstream refusals (the routing prerequisites, issues #85
-- and #86's vocabulary half):
--
--   A request whose execution ends BEFORE commitment because the upstream
--   refused it is a terminal outcome the row must be able to name. The
--   routing disposition table (docs/architecture/routing.md) classifies three
--   such refusals — the upstream rejecting the forwarded request, a context
--   too large for the upstream, and the upstream refusing our credentials —
--   as non-retryable and non-fallback: they are surfaced to the client, which
--   makes them terminal request outcomes. Until this file, the vocabulary was
--   closed at two values, `stream_failed_after_commitment` and
--   `gateway_abandoned`, and none of the three had a spelling.
--
--   The REPLAY CONTRACT is why each refusal needs its own name rather than a
--   shared "upstream refused" word: the runtime's idempotent-replay promise
--   (api/openapi/shared/runtime-errors.yaml, "Idempotent replay") is that the
--   replayed body is byte-for-byte the answer the original request received.
--   A refusal's answer is distinct from every other failure's, so the replay
--   record's `final_failure_reason` must carry WHICH refusal happened for the
--   probe to re-derive the original answer. One shared value would collapse
--   the three answers into one and break the promise by construction.
--
--   THE VALUES, in both lists this file widens:
--
--     provider_rejected_request   the upstream refused the forwarded request;
--                                 the client's request is at fault and the
--                                 answer says so.
--     context_too_large           the request's context exceeds what the
--                                 upstream accepts.
--     upstream_authentication     the upstream refused our credentials — an
--                                 operator problem, surfaced loudly.
--
--   All three are PRE-COMMITMENT endings: commitment is the first
--   content-bearing byte, and a refusal arrives before any byte was
--   forwarded, so like `gateway_abandoned` they name no committed attempt.
--   The existing `requests_failure_reason_attempt_pairing` constraint already
--   enforces exactly that shape and is deliberately NOT rewritten here: for
--   any `failure_reason` other than `stream_failed_after_commitment` its
--   equality demands `committed_attempt_id IS NULL`, which is what the three
--   new values require, while its `failure_reason IS NULL` arm keeps admitting
--   the succeeded row, whose committed attempt is named and whose failure
--   reason is NULL. Widening the lists is the entire schema contribution.
--
--   The domain twin moves with this file:
--   `apps/dataplane/internal/domain/execution` gains the three constants and
--   the `FailBeforeCommitment` transition that is the only writer of them —
--   the package refuses in Go what this file's CHECKs would refuse again in
--   SQL, which is the doubling the runtime's storage foundation promises
--   (migration 000003's header; request-lifecycle.md's "landed code, not a
--   glossary").
--
-- DOWN AND THE VOCABULARY. The down migration restores the two-value lists.
-- Re-adding a CHECK validates the rows the table already holds, so a
-- database that has recorded rows under the widened vocabulary must rewrite
-- those rows to the restored vocabulary (or be emptied of them) before the
-- down applies. That is the down's contract, not a defect: a rollback that
-- silently rewrote terminal outcomes would be inventing answers the runtime
-- never gave.

ALTER TABLE public.requests
    DROP CONSTRAINT requests_failed_shape;

ALTER TABLE public.requests
    ADD CONSTRAINT requests_failed_shape CHECK (
        status <> 'failed'
        OR (
            rejection_reason IS NULL
            AND failure_reason IN (
                'stream_failed_after_commitment',
                'gateway_abandoned',
                'provider_rejected_request',
                'context_too_large',
                'upstream_authentication'
            )
            AND finished_at IS NOT NULL
        )
    );

ALTER TABLE public.request_intake
    DROP CONSTRAINT request_intake_final_failure_reason_check;

ALTER TABLE public.request_intake
    ADD CONSTRAINT request_intake_final_failure_reason_check CHECK (
        final_failure_reason IS NULL
        OR final_failure_reason IN (
            'stream_failed_after_commitment',
            'gateway_abandoned',
            'provider_rejected_request',
            'context_too_large',
            'upstream_authentication'
        )
    );
