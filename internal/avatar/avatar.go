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
// page: an <img> pointing at them tells that provider the viewer's address,
// their user agent and, through the Referer, which page of this product they
// are reading — on every load. The picture is therefore fetched here once,
// checked, shrunk and stored, and the browser only ever talks to us. Google's
// URL rotates besides, so keeping one would not have worked anyway.
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

// MaxDownloadBytes is the most we will read from a provider. Without it, a
// response we did not write decides how much memory this process spends.
const MaxDownloadBytes = 2 << 20

// MaxDimension is the largest edge we store. Every surface renders the picture
// at a few dozen pixels, so anything larger is bytes nobody sees, on a row read
// on every dashboard load.
const MaxDimension = 512

// MaxPixels is the largest picture we will decode, about 2048 on a side. The
// byte cap bounds the download, not the decode: a few kilobytes of PNG can
// describe a canvas tens of thousands of pixels a side, which is gigabytes the
// moment anything reads it.
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

// ErrNoImage means the provider said clearly that it has no picture for this
// person. Everything else — a timeout, a 500, a rate limit — is an ordinary
// error, so a provider having a bad day is never recorded as a person having no
// picture.
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

	// Only an explicit "there is nothing here" is a miss. A redirect arrives as
	// a 3xx because the outbound client refuses to follow one — the second
	// destination is one nobody validated — and that is a failure, not an
	// answer.
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		return Image{}, ErrNoImage
	}
	if response.StatusCode != http.StatusOK {
		return Image{}, fmt.Errorf("avatar: the provider answered %d", response.StatusCode)
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
// Decoding is the type check — a provider's Content-Type is a claim — and
// re-encoding is what makes it worth something: the bytes we serve are ones
// this process produced, so anything smuggled in the parts of a file the
// decoder ignores does not survive.
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

// Store reads and writes stored pictures on the system database.
type Store struct {
	db  *sql.DB
	now func() int64
}

// NewStore builds the store over the system database.
func NewStore(db *sql.DB, now func() int64) *Store {
	return &Store{db: db, now: now}
}

// State is what is known about a person's picture without loading it. It is a
// separate read from the picture itself because the pages that decide whether
// to show one never need the bytes.
type State struct {
	// ETag is empty when there is nothing to serve.
	ETag string

	// Source names the provider a stored picture came from.
	Source string

	// Asked reports that a provider has already answered, whether or not it had
	// anything. It is what stops an address with no Gravatar costing an
	// outbound request on every sign-in for ever.
	Asked bool
}

// State reads one person's picture status. A person with no row is not an
// error: having no picture is the common case, not a failure.
func (s *Store) State(ctx context.Context, userID int64) (State, error) {
	var state State

	err := s.db.QueryRowContext(ctx, `
		SELECT etag, source FROM user_avatars WHERE user_id = ?
	`, userID).Scan(&state.ETag, &state.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("avatar: read state: %w", err)
	}

	state.Asked = true

	return state, nil
}

// Read returns one person's stored picture, or an empty Image when there is
// none. The caller renders the letter, which is a fine default.
func (s *Store) Read(ctx context.Context, userID int64) (Image, error) {
	var (
		raw         []byte
		contentType string
		etag        string
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(bytes, x''), type, etag FROM user_avatars WHERE user_id = ?
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
		INSERT INTO user_avatars (user_id, bytes, type, etag, source, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			bytes = excluded.bytes, type = excluded.type, etag = excluded.etag,
			source = excluded.source, fetched_at = excluded.fetched_at
	`, userID, picture.Bytes, picture.Type, picture.ETag, source, s.now())
	if err != nil {
		return fmt.Errorf("avatar: save: %w", err)
	}

	return nil
}

// RememberMiss records that a provider had nothing for this person.
//
// It deliberately leaves any bytes already stored alone. A provider answering
// "no picture" is not a reason to throw away one somebody already has, and
// without that rule a single 404 from one source erases what another supplied.
func (s *Store) RememberMiss(ctx context.Context, userID int64, source string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_avatars (user_id, type, etag, source, fetched_at)
		VALUES (?, '', '', ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET source = excluded.source, fetched_at = excluded.fetched_at
	`, userID, source, s.now())
	if err != nil {
		return fmt.Errorf("avatar: remember miss: %w", err)
	}

	return nil
}
