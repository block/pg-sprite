-- Disposable test credentials only. The stock image creates these roles.
ALTER ROLE supabase_auth_admin PASSWORD 'pgsprite_test_only';
ALTER ROLE authenticator PASSWORD 'pgsprite_test_only';
ALTER ROLE pgbouncer PASSWORD 'pgsprite_test_only';
CREATE DATABASE _supabase OWNER supabase_admin;
\connect _supabase
CREATE SCHEMA IF NOT EXISTS _supavisor AUTHORIZATION supabase_admin;
