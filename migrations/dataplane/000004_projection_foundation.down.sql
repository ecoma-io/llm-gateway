-- The inverse of 000004_projection_foundation.up.sql, in reverse creation
-- order, no CASCADE. Every row in these tables is the Control Plane's
-- projection, re-derivable by a snapshot and an incremental replay; dropping
-- them removes a mirror, not an authority.

DROP TABLE projection_state;
DROP TABLE account_states;
DROP TABLE api_key_credentials;
