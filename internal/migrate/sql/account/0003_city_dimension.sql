--
-- 0003_city_dimension.sql
-- City becomes an interned name, like every other place we group by.
--
-- Created: 2026-08-30
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- The first migration stored a GeoNames id for the city on the theory that the
-- geolocation database already hands us a stable integer. The database we
-- actually ship does not: DB-IP Lite carries no geoname ids anywhere, so the
-- column could only ever hold 0 and the city was missing from every event.
--
-- Interning the name instead makes city an ordinary dimension. It also removes
-- a table we would otherwise have to ship: with an id, the dashboard needs a
-- GeoNames id-to-name file just to render "London", and that file is another
-- 100 MB download with its own licence. With the name, the dim_city row is the
-- answer.
--
-- The old ids are dropped rather than translated. A geoname id is meaningless
-- as a dim_city id, and turning one into a name needs exactly the GeoNames
-- table this change exists to avoid; any pre-existing row therefore comes out
-- of this migration with no city, which is what it effectively had anyway.
--
-- dim_region holds a human-readable name — "England", not "GB-ENG". DB-IP Lite
-- names its subdivisions and never codes them, so a name is the only thing
-- there is to store for most of the world, and it is what a dashboard shows
-- without a second lookup. Where a database does supply an ISO-3166-2 code the
-- reader still prefers it, so the column holds a mix by design: a code where
-- one exists, a name where one does not, and both are display-ready.

CREATE TABLE dim_city (id INTEGER PRIMARY KEY, value TEXT NOT NULL UNIQUE);

-- Id 0 is the empty string in every dimension table, so "not set" stays an
-- ordinary id rather than a NULL every query would have to branch on.
INSERT INTO dim_city (id, value) VALUES (0, '');

ALTER TABLE events DROP COLUMN city_geoname_id;
ALTER TABLE events ADD COLUMN city_id INTEGER NOT NULL DEFAULT 0;

ALTER TABLE sessions DROP COLUMN city_geoname_id;
ALTER TABLE sessions ADD COLUMN city_id INTEGER NOT NULL DEFAULT 0;
