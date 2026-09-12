-- Trigram index support for athlete name search. A B-tree cannot serve
-- ILIKE '%doe%', which is what the dashboard's search box issues.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
