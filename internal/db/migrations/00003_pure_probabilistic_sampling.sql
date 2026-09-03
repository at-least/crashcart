-- Sampling drops its per-issue "first N always kept" guarantee: every
-- event is now an independent sample_rate coin flip (unhandled crashes get
-- UnhandledKeepFactor× that rate, capped at 1), so a real issue surfaces
-- through volume instead of a stored count. See ARCHITECTURE.md.
ALTER TABLE projects DROP COLUMN sample_keep_first;
