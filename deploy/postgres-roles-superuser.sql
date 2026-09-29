\set ON_ERROR_STOP on

-- Run once as a superuser (or the managed provider's administrative role)
-- after postgres-roles.sql has created the group roles. PostgreSQL only lets a
-- superuser grant SET on a superuser-context parameter, so the database owner
-- cannot apply this line; keeping it here lets the owner script finish and
-- apply the default privileges that follow it.
--
-- The restore role is activated only for an approved isolated logical restore.
-- Trigger suppression is needed to load the tenant FK graph without relying on
-- table order; the application restores origin mode and validates every
-- tenant FK before commit. Skip this file if logical tenant restore is unused.
GRANT SET ON PARAMETER session_replication_role TO tutor_restore;
