-- The deliberately-failing migration of the integration suite — a fixture,
-- not schema. verify.sh copies the repository's real migrations into a
-- temporary directory, adds this pair, and mounts that directory over the
-- migration runner's source for exactly one step. Applied history under
-- migrations/ is immutable; this is how the suite runs a migration that
-- fails without ever putting one there.
--
-- The failure is engineered for the safety model's central claims: statement
-- one succeeds, statement two fails — at execution, not at parse — so the
-- single-transaction delivery must roll statement one back. If this file
-- ever applies cleanly, the suite itself fails at step 9/11.
CREATE TABLE public._verify_failed_probe (
    id integer NOT NULL PRIMARY KEY
);

-- Parses fine, fails on execution: 'not-an-integer' cannot be cast to the
-- integer column created above.
INSERT INTO public._verify_failed_probe (id) VALUES ('not-an-integer');
