-- Unreachable in the suite: the up migration of this pair cannot complete,
-- so nothing is ever rolled back from it. The file exists because the
-- migration tool requires a paired down file before it will consider the
-- pair valid at all — and because every migration ships with its inverse,
-- fixtures included.
DROP TABLE IF EXISTS public._verify_failed_probe;
