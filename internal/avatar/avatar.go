//
// avatar.go
// Account pictures, fetched once and served from our own origin.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

// Package avatar turns a picture belonging to a person into bytes we hold.
//
// Google and Gravatar both serve images, and neither may be linked to from a
// page. An <img> pointing at them makes the viewer's browser tell that provider
// the viewer's IP address, their user agent, and — through the Referer — which
// page of this product they are looking at. Doing that on every load of a
// privacy-first analytics product is the exact behaviour our customers switched
// to us to avoid. Google's picture URL also rotates, so a stored URL rots.
//
// So the picture is fetched here, once, checked, shrunk, and written to the
// system database. The browser only ever talks to us.
package avatar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"strings"

	"golang.org/x/image/draw"

	// WebP is decode-only in Go, which is enough: everything is re-encoded
	// below, so a WebP that arrives leaves as a PNG.
	_ "golang.org/x/image/webp"
)

// MaxDownloadBytes is the most we will read from a provider.
//
// A profile picture is tens of kilobytes. The cap exists because the response
// is a stranger's: without it, a provider — or anything that can answer as one
// — decides how much memory this process spends.
const MaxDownloadBytes = 2 << 20

// MaxDimension is the largest edge we store. Every surface renders the picture
// at a few dozen pixels, so anything larger is bytes nobody sees, on a row read
// on every dashboard load.
const MaxDimension = 512

// MaxPixels is the largest picture we will decode, about 2048 on a side.
//
// The byte cap above bounds the download, not the decode, and those are very
// different numbers: a few kilobytes of PNG can describe a canvas of tens of
// thousands of pixels a side, which becomes gigabytes of memory the moment
// anything reads it. The header is read first and a picture over this is
// refused before a single row of it is allocated.
const MaxPixels = 4 << 20

// Image is a picture ready to be stored and served.
type Image struct {
	Bytes []byte

	// Type is the response Content-Type, always one of the encodings below.
	Type string

	// ETag is the strong validator for the bytes, so a browser that already
	// has this picture is answered with a 304 rather than the image again.
	ETag string
}

// SourceGoogle and SourceGravatar name where a stored picture came from. A
// Google picture is the person's own choice inside an account they are signed
// in to, so it outranks a Gravatar derived from their address.
const (
	SourceGoogle   = "google"
	SourceGravatar = "gravatar"
)

// ErrNoImage means the provider had nothing for this person. It is an ordinary
// answer, not a failure: Gravatar is asked with d=404 precisely so that "no
// picture" arrives as a clean miss rather than as a generated default we would
// then store and serve as though somebody had chosen it.
var ErrNoImage = errors.New("avatar: the provider has no picture")

// GravatarURL is where to ask for the picture behind an email address.
//
// The address is lower-cased and trimmed before hashing because that is the
// scheme Gravatar specifies; hashing the address as typed silently misses. The
// d=404 is what makes a missing picture a miss instead of a generated pattern,
// and s= asks for the size we intend to keep rather than the full original.
func GravatarURL(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))

	return fmt.Sprintf("https://gravatar.com/avatar/%s?d=404&s=%d", hex.EncodeToString(sum[:]), MaxDimension)
}

// Fetch downloads one picture and normalises it.
//
// The client is the caller's, so the outbound policy that guards every other
// request this process makes on somebody else's say-so guards this one too.
func Fetch(ctx context.Context, client *http.Client, url string) (Image, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Image{}, fmt.Errorf("avatar: build request: %w", err)
	}

	response, err := client.Do(request)
	if err != nil {
		return Image{}, fmt.Errorf("avatar: fetch: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck // the body is fully read or abandoned

	// A redirect arrives here as a 3xx because the outbound client refuses to
	// follow one: the second destination is one nobody validated.
	if response.StatusCode != http.StatusOK {
		return Image{}, ErrNoImage
	}

	// One byte past the cap is read so a body that is exactly at the limit is
	// distinguishable from one that is over it.
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxDownloadBytes+1))
	if err != nil {
		return Image{}, fmt.Errorf("avatar: read: %w", err)
	}
	if len(raw) > MaxDownloadBytes {
		return Image{}, fmt.Errorf("avatar: picture is larger than %d bytes", MaxDownloadBytes)
	}

	return Normalise(raw)
}

// Normalise decodes, shrinks and re-encodes one downloaded picture.
//
// Decoding is the check: a byte string that is not a picture in a format we
// recognise never becomes one, whatever the provider called it. Re-encoding is
// what makes that check worth something — the bytes we serve are ones this
// process produced, so a payload smuggled in the parts of a file the decoder
// ignores does not survive.
func Normalise(raw []byte) (Image, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return Image{}, fmt.Errorf("avatar: %w", err)
	}

	if format != "png" && format != "jpeg" && format != "webp" {
		return Image{}, fmt.Errorf("avatar: %s is not a picture format we store", format)
	}

	if config.Width < 1 || config.Height < 1 || config.Width*config.Height > MaxPixels {
		return Image{}, fmt.Errorf("avatar: %dx%d is not a size we decode", config.Width, config.Height)
	}

	source, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Image{}, fmt.Errorf("avatar: %w", err)
	}

	encoded, contentType, err := encode(shrink(source), format)
	if err != nil {
		return Image{}, err
	}

	sum := sha256.Sum256(encoded)

	return Image{Bytes: encoded, Type: contentType, ETag: `"` + hex.EncodeToString(sum[:16]) + `"`}, nil
}

// shrink scales a picture down to fit MaxDimension, keeping its proportions. A
// picture already small enough is returned untouched: scaling it up would cost
// bytes and quality to gain nothing.
func shrink(source image.Image) image.Image {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	if width <= MaxDimension && height <= MaxDimension {
		return source
	}

	if width > height {
		height = height * MaxDimension / width
		width = MaxDimension
	} else {
		width = width * MaxDimension / height
		height = MaxDimension
	}

	// A one-pixel edge is still a picture; a zero-pixel one is an encoder error
	// on a very long thin image.
	width = max(width, 1)
	height = max(height, 1)

	target := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(target, target.Bounds(), source, bounds, draw.Over, nil)

	return target
}

// encode writes the picture back out. JPEG stays JPEG because re-encoding a
// photograph as PNG multiplies its size; everything else becomes PNG, which is
// lossless and keeps the transparency a cropped avatar usually has.
func encode(source image.Image, format string) ([]byte, string, error) {
	var out bytes.Buffer

	if format == "jpeg" {
		if err := jpeg.Encode(&out, source, &jpeg.Options{Quality: 85}); err != nil {
			return nil, "", fmt.Errorf("avatar: encode jpeg: %w", err)
		}

		return out.Bytes(), "image/jpeg", nil
	}

	if err := png.Encode(&out, source); err != nil {
		return nil, "", fmt.Errorf("avatar: encode png: %w", err)
	}

	return out.Bytes(), "image/png", nil
}

// Store reads and writes the avatar columns on the system database.
type Store struct {
	db  *sql.DB
	now func() int64
}

// NewStore builds the store over the system database.
func NewStore(db *sql.DB, now func() int64) *Store {
	return &Store{db: db, now: now}
}

// Read returns one person's stored picture. A person with no picture, or with a
// remembered miss, comes back as an empty Image and no error: the caller
// renders the letter, which is a fine default rather than a failure.
func (s *Store) Read(ctx context.Context, userID int64) (Image, error) {
	var (
		raw         []byte
		contentType string
		etag        string
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(avatar_bytes, x''), avatar_type, avatar_etag FROM users WHERE id = ?
	`, userID).Scan(&raw, &contentType, &etag)
	if errors.Is(err, sql.ErrNoRows) {
		return Image{}, nil
	}
	if err != nil {
		return Image{}, fmt.Errorf("avatar: read: %w", err)
	}

	if len(raw) == 0 || etag == "" {
		return Image{}, nil
	}

	return Image{Bytes: raw, Type: contentType, ETag: etag}, nil
}

// Save stores a fetched picture against a person.
func (s *Store) Save(ctx context.Context, userID int64, source string, picture Image) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET avatar_bytes = ?, avatar_type = ?, avatar_etag = ?, avatar_source = ?, avatar_fetched_at = ?
		WHERE id = ?
	`, picture.Bytes, picture.Type, picture.ETag, source, s.now(), userID)
	if err != nil {
		return fmt.Errorf("avatar: save: %w", err)
	}

	return nil
}

// RememberMiss records that a provider had nothing, so the next page load does
// not ask again. Silence from a provider is an answer worth keeping.
func (s *Store) RememberMiss(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET avatar_bytes = NULL, avatar_type = '', avatar_etag = '', avatar_source = '', avatar_fetched_at = ?
		WHERE id = ?
	`, s.now(), userID)
	if err != nil {
		return fmt.Errorf("avatar: remember miss: %w", err)
	}

	return nil
}
