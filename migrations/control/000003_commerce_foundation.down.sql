-- The exact inverse of 000003_commerce_foundation.up.sql, in reverse creation
-- order: tables drop children-first so the foreign keys they carry never
-- dangle, and the indexes drop with their tables. No CASCADE anywhere — the
-- up file refuses to let the schema be torn through, and so does the down.
-- No IF EXISTS either, holding the lane's convention: this runs against a
-- database whose history says these tables were applied, so one that lacks
-- them is an unexpected fact the rollback should fail on rather than paper
-- over.
--
-- This drops the Control Plane's entire commerce foundation: plans and their
-- versions, the grant definitions, every subscription, entitlement and PAYG
-- flag row. That is what "down" means for this file, and it is why down files
-- are executed by deploy/postgres/verify.sh and by nothing that serves
-- traffic.
--
-- Like the up file, this file carries no BEGIN, COMMIT or ROLLBACK of its
-- own, per migrations/README.md.

DROP TABLE control.account_payg;

DROP TABLE control.entitlements;

DROP TABLE control.subscriptions;

DROP TABLE control.plan_grant_definitions;

DROP TABLE control.plan_versions;

DROP TABLE control.plans;
