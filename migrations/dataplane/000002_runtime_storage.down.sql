-- The exact inverse of 000002_runtime_storage.up.sql, in dependency order.
--
-- The trigger function is dropped explicitly: a DROP TABLE removes its
-- triggers but NOT the function they reference, and an orphaned function in
-- public is exactly the kind of leftover a down is sworn not to leave (the
-- persistence suite asserts public holds nothing but its migration history
-- after a full roll-back).
--
-- usage_events goes first: it references requests, request_attempts and
-- itself. The stream table has no dependents. reservation_allocations goes
-- before reservations; quota_projections and request_intake stand alone (the
-- intake's request_id is deliberately an ID reference — enforced keys stop at
-- the family boundary, ADR 0005).
--
-- requests and request_attempts form the one cycle, through the composite
-- requests_committed_attempt_fkey. A plain pair of DROPs fails on it in
-- either order ("cannot drop table ... because other objects depend on it");
-- dropping that one constraint first is what makes the rest order-free.

DROP TRIGGER requests_terminal_immutability ON public.requests;
DROP TRIGGER reservations_terminal_immutability ON public.reservations;
DROP FUNCTION public.reject_terminal_row_update();

DROP TABLE public.usage_events;
DROP TABLE public.usage_events_stream;
DROP TABLE public.reservation_allocations;
DROP TABLE public.reservations;
DROP TABLE public.quota_projections;
DROP TABLE public.request_intake;

ALTER TABLE public.requests DROP CONSTRAINT requests_committed_attempt_fkey;
DROP TABLE public.request_attempts;
DROP TABLE public.requests;
