//
// avatar_test.go
// What we will accept from a provider, and what we store when we do.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package avatar

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Feasible-Analytics/app.feasible.lol/internal/migrate"
	"github.com/Feasible-Analytics/app.feasible.lol/internal/store"
)

// square builds a picture of the given size. The colour varies across the image
// so a scaler has something to average and an encoder something to compress.
func square(t *testing.T, size int, encode string) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}

	var out bytes.Buffer
	var err error

	if encode == "jpeg" {
		err = jpeg.Encode(&out, img, nil)
	} else {
		err = png.Encode(&out, img)
	}
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}

	return out.Bytes()
}

// TestAnOversizedPictureIsShrunkRatherThanStoredAsSent keeps the row small. The
// picture renders at a few dozen pixels everywhere it appears, so a provider's
// full-resolution original is bytes nobody ever sees on a row that is read on
// every page load.
func TestAnOversizedPictureIsShrunkRatherThanStoredAsSent(t *testing.T) {
	picture, err := Normalise(square(t, 1200, "png"))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}

	decoded, _, err := image.Decode(bytes.NewReader(picture.Bytes))
	if err != nil {
		t.Fatalf("decode stored picture: %v", err)
	}

	if got := decoded.Bounds().Dx(); got != MaxDimension {
		t.Errorf("stored width = %d, want %d", got, MaxDimension)
	}
	if got := decoded.Bounds().Dy(); got != MaxDimension {
		t.Errorf("stored height = %d, want %d", got, MaxDimension)
	}
}

// TestASmallPictureIsLeftAlone checks the other half. Scaling a 64-pixel avatar
// up to 512 would cost bytes and sharpness to gain nothing.
func TestASmallPictureIsLeftAlone(t *testing.T) {
	picture, err := Normalise(square(t, 64, "png"))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}

	decoded, _, err := image.Decode(bytes.NewReader(picture.Bytes))
	if err != nil {
		t.Fatalf("decode stored picture: %v", err)
	}

	if got := decoded.Bounds().Dx(); got != 64 {
		t.Errorf("stored width = %d, want the original 64", got)
	}
}

// TestAPhotographStaysAJpeg checks the encoding choice. Re-encoding a photo as
// PNG multiplies its size for no benefit, and these rows are read on every page
// load.
func TestAPhotographStaysAJpeg(t *testing.T) {
	picture, err := Normalise(square(t, 600, "jpeg"))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}

	if picture.Type != "image/jpeg" {
		t.Errorf("stored type = %q, want image/jpeg", picture.Type)
	}

	png, err := Normalise(square(t, 600, "png"))
	if err != nil {
		t.Fatalf("normalise png: %v", err)
	}

	if png.Type != "image/png" {
		t.Errorf("stored type = %q, want image/png", png.Type)
	}
}

// TestSomethingThatIsNotAPictureIsRefused is the type check. Decoding is what
// performs it: a provider's Content-Type is a claim, and the only thing that
// proves a byte string is a picture is decoding it as one.
func TestSomethingThatIsNotAPictureIsRefused(t *testing.T) {
	for name, raw := range map[string][]byte{
		"html":  []byte("<!doctype html><title>not a picture</title>"),
		"empty": {},
		"svg":   []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
	} {
		if _, err := Normalise(raw); err == nil {
			t.Errorf("%s was accepted as a picture", name)
		}
	}
}

// TestADecompressionBombIsRefusedFromItsHeader is the limit that matters most.
// The download cap bounds the bytes on the wire, not the memory a decoder
// spends: a few kilobytes of PNG can describe a canvas tens of thousands of
// pixels on a side, and reading it is gigabytes.
func TestADecompressionBombIsRefusedFromItsHeader(t *testing.T) {
	// A single-colour PNG compresses to almost nothing, which is exactly what
	// makes this shape dangerous.
	bomb := image.NewRGBA(image.Rect(0, 0, 20000, 20000))

	var encoded bytes.Buffer
	if err := png.Encode(&encoded, bomb); err != nil {
		t.Fatalf("encode the fixture: %v", err)
	}

	if encoded.Len() > MaxDownloadBytes {
		t.Fatalf("the fixture is %d bytes, which the download cap would catch first", encoded.Len())
	}

	if _, err := Normalise(encoded.Bytes()); err == nil {
		t.Fatal("a 20000x20000 picture was decoded")
	}
}

// TestTheEtagFollowsTheBytes is what makes the cache header safe. The URL is
// stable, so a browser holding an old picture is only corrected because the
// validator changed with it.
func TestTheEtagFollowsTheBytes(t *testing.T) {
	first, err := Normalise(square(t, 64, "png"))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}

	same, err := Normalise(square(t, 64, "png"))
	if err != nil {
		t.Fatalf("normalise again: %v", err)
	}

	other, err := Normalise(square(t, 96, "png"))
	if err != nil {
		t.Fatalf("normalise other: %v", err)
	}

	if first.ETag != same.ETag {
		t.Errorf("the same picture produced %q and %q", first.ETag, same.ETag)
	}
	if first.ETag == other.ETag {
		t.Errorf("two different pictures share the tag %q", first.ETag)
	}
	if !strings.HasPrefix(first.ETag, `"`) || !strings.HasSuffix(first.ETag, `"`) {
		t.Errorf("tag %q is not quoted, which makes it an invalid ETag", first.ETag)
	}
}

// TestGravatarAsksForACleanMiss checks the two query parameters that matter.
// Without d=404 a missing picture arrives as a generated pattern, which we
// would then store and serve as though somebody had chosen it.
func TestGravatarAsksForACleanMiss(t *testing.T) {
	url := GravatarURL("  Spicer@Example.COM ")

	if !strings.Contains(url, "d=404") {
		t.Errorf("gravatar URL %q does not ask for a clean miss", url)
	}

	// The address is lower-cased and trimmed before hashing because that is the
	// scheme Gravatar specifies; hashing it as typed silently never matches.
	if url != GravatarURL("spicer@example.com") {
		t.Errorf("a differently typed address produced a different URL: %q", url)
	}
}

// newStore builds a store over a migrated system database with one person in
// it, which is what every write below needs.
func newStore(t *testing.T) (*Store, *sql.DB, int64) {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatalf("open system database: %v", err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck // a closed test database needs no assertion

	if _, err := migrate.Run(context.Background(), db, migrate.System()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	result, err := db.Exec(`INSERT INTO users (email, name, created_at, updated_at) VALUES ('a@example.com', 'A', 1, 1)`)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	id, _ := result.LastInsertId()

	return NewStore(db, func() int64 { return 1700000000 }), db, id
}

// TestAStoredPictureComesBackWhole is the round trip the route depends on.
func TestAStoredPictureComesBackWhole(t *testing.T) {
	avatars, _, userID := newStore(t)
	ctx := context.Background()

	picture, err := Normalise(square(t, 64, "png"))
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}

	if err := avatars.Save(ctx, userID, SourceGoogle, picture); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := avatars.Read(ctx, userID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if !bytes.Equal(got.Bytes, picture.Bytes) || got.Type != picture.Type || got.ETag != picture.ETag {
		t.Errorf("stored picture came back as %d bytes / %q / %q", len(got.Bytes), got.Type, got.ETag)
	}
}

// TestAMissIsRememberedAndReadsAsNoPicture is what stops an address with no
// Gravatar costing an outbound request on every sign-in for ever.
func TestAMissIsRememberedAndReadsAsNoPicture(t *testing.T) {
	avatars, db, userID := newStore(t)
	ctx := context.Background()

	if err := avatars.RememberMiss(ctx, userID); err != nil {
		t.Fatalf("remember miss: %v", err)
	}

	got, err := avatars.Read(ctx, userID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Bytes) != 0 || got.ETag != "" {
		t.Errorf("a remembered miss read back as a picture: %d bytes, tag %q", len(got.Bytes), got.ETag)
	}

	var fetchedAt sql.NullInt64
	if err := db.QueryRow(`SELECT avatar_fetched_at FROM users WHERE id = ?`, userID).Scan(&fetchedAt); err != nil {
		t.Fatalf("read fetched_at: %v", err)
	}
	if !fetchedAt.Valid {
		t.Error("a miss left no record that the provider had been asked")
	}
}

// TestReadingSomebodyWithNoPictureIsNotAnError keeps the letter circle an
// ordinary answer. A person who never signed in with Google and has no Gravatar
// is the common case, not a failure.
func TestReadingSomebodyWithNoPictureIsNotAnError(t *testing.T) {
	avatars, _, userID := newStore(t)

	got, err := avatars.Read(context.Background(), userID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Bytes) != 0 {
		t.Errorf("a person with no picture read back %d bytes", len(got.Bytes))
	}

	missing, err := avatars.Read(context.Background(), userID+9999)
	if err != nil {
		t.Fatalf("read a person who does not exist: %v", err)
	}
	if len(missing.Bytes) != 0 {
		t.Error("a person who does not exist read back a picture")
	}
}
