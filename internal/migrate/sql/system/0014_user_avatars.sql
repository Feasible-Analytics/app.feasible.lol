--
-- 0014_user_avatars.sql
-- Each person's picture, as bytes we serve ourselves.
--
-- Created: 2026-09-02
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- The picture comes from the provider a person signed in with, which may not
-- be linked to from a page: a browser fetching an image hands that provider the
-- viewer's address, their user agent and the page they are on, on every load.
--
-- It is its own table rather than columns on users because users is read on
-- every authenticated request. A blob in that row is fifty kilobytes SQLite
-- steps past, through its overflow pages, to reach the columns the request
-- actually wanted. It also leaves room for the picture somebody uploads
-- themselves, which needs no new shape.

CREATE TABLE user_avatars (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,

    -- Empty when a provider was asked and had no picture.
    bytes      BLOB,
    type       TEXT NOT NULL DEFAULT '',

    -- The strong validator for the bytes, so a browser that already holds this
    -- picture is answered with a 304 rather than the image again.
    etag       TEXT NOT NULL DEFAULT '',

    -- Which provider was asked.
    source     TEXT NOT NULL DEFAULT '',

    fetched_at INTEGER NOT NULL
);
