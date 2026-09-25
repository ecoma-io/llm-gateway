-- The exact inverse of 000003_runtime_storage.up.sql, in dependency order.
--
-- Every trigger function is dropped explicitly: a DROP TABLE removes its
-- triggers but NOT the functions they reference, and an orphaned function in
-- public is exactly the kind of leftover a down is sworn not to leave.
-- deploy/postgres/verify.sh asserts the lane rolls back to version 0 with the
-- extension gone; the suite's fresh-apply proof re-derives the same shape on
-- every run.
--
-- usage_events goes first: it references requests, request_attempts and
-- itself. The stream table has no dependents. quota_refills goes before
-- quota_projections (it references the projection's bucket key);
-- reservation_allocations goes before reservations; request_intake stands
-- alone (the intake's request_id is deliberately an ID reference — enforced
-- keys stop at the family boundary, ADR 0005).
--
-- requests and request_attempts form the one cycle, through the composite
-- requests_committed_attempt_fkey. A plain pair of DROPs fails on it in
-- either order ("cannot drop table ... because other objects depend on it");
-- dropping that one constraint first is what makes the rest order-free.

DROP TRIGGER requests_terminal_immutability ON public.requests;
DROP TRIGGER reservations_terminal_immutability ON public.reservations;
DROP TRIGGER request_intake_identity_immutability ON public.request_intake;
DROP TRIGGER reservation_allocations_rederive_the_hold ON public.reservation_allocations;
DROP TRIGGER reservation_allocations_immutability ON public.reservation_allocations;
DROP TRIGGER usage_events_payload_envelope ON public.usage_events;
DROP FUNCTION public.reject_terminal_row_update();
DROP FUNCTION public.reject_intake_identity_rewrite();
DROP FUNCTION public.reject_legs_that_do_not_rederive_the_hold();
DROP FUNCTION public.reject_allocation_leg_rewrite();
DROP FUNCTION public.reject_undeliverable_fact_payload();

DROP TABLE public.usage_events;
DROP TABLE public.usage_events_stream;
DROP TABLE public.quota_refills;
DROP TABLE public.reservation_allocations;
DROP TABLE public.reservations;
DROP TABLE public.quota_projections;
DROP TABLE public.request_intake;

ALTER TABLE public.requests DROP CONSTRAINT requests_committed_attempt_fkey;
DROP TABLE public.request_attempts;
DROP TABLE public.requests;
