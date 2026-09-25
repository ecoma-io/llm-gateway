-- Down: restore the two-value failure vocabulary of 000003_runtime_storage.
-- See this migration's up header: re-adding these CHECKs validates the rows
-- already present, so a database holding rows under the widened vocabulary
-- (`provider_rejected_request`, `context_too_large`,
-- `upstream_authentication`) must rewrite or shed those rows first. No
-- CASCADE, no data rewrite happens here — the constraint restoration refuses
-- such a database rather than silently reinterpreting its rows.

ALTER TABLE public.requests
    DROP CONSTRAINT requests_failed_shape;

ALTER TABLE public.requests
    ADD CONSTRAINT requests_failed_shape CHECK (
        status <> 'failed'
        OR (
            rejection_reason IS NULL
            AND failure_reason IN ('stream_failed_after_commitment', 'gateway_abandoned')
            AND finished_at IS NOT NULL
        )
    );

ALTER TABLE public.request_intake
    DROP CONSTRAINT request_intake_final_failure_reason_check;

ALTER TABLE public.request_intake
    ADD CONSTRAINT request_intake_final_failure_reason_check CHECK (
        final_failure_reason IS NULL
        OR final_failure_reason IN ('stream_failed_after_commitment', 'gateway_abandoned')
    );
