--
-- 0014_user_avatars.sql
-- Each person's picture, as bytes we serve ourselves.
--
-- Created: 2026-09-02
-- Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
--
-- The picture comes from Google or Gravatar, and neither may be linked to from
-- a page: a browser fetching an image hands that provider the viewer's address,
-- their user agent and the page they are on, on every load.
--
-- It is its own table rather than columns on users because users is read on
-- every authenticated request. A blob in that row is fifty kilobytes SQLite
-- steps past, through its overflow pages, to reach the columns the request
-- actually wanted. It also leaves room for the picture somebody uploads
-- themselves, which needs no new shape.

CREATE TABLE user_avatars (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,

    -- Empty for a remembered miss: an address with no Gravatar. The row exists
    -- so the provider is not asked again on every sign-in for ever.
    bytes      BLOB,
    type       TEXT NOT NULL DEFAULT '',

    -- The strong validator for the bytes, so a browser that already holds this
    -- picture is answered with a 304 rather than the image again.
    etag       TEXT NOT NULL DEFAULT '',

    -- Which provider was asked. A Google picture is a choice the person made
    -- inside an account they are signed in to, so it outranks a Gravatar
    -- derived from their address.
    source     TEXT NOT NULL DEFAULT '',

    fetched_at INTEGER NOT NULL
);
