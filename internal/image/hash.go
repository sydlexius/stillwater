package image

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/image/draw"
)

// PerceptualHash computes a 64-bit dHash (difference hash) from an image.
// The image is resized to 9x8 grayscale, then each pixel is compared to
// its right neighbor. If the left pixel is brighter, the corresponding
// bit is set. This produces a hash that is robust against scaling, minor
// color adjustments, and JPEG compression artifacts.
func PerceptualHash(r io.Reader) (uint64, error) {
	src, err := decodeWithLimit(r)
	if err != nil {
		return 0, fmt.Errorf("decoding image for hash: %w", err)
	}

	return PerceptualHashFromImage(src), nil
}

// PerceptualHashFromImage computes a dHash from an already-decoded image.
func PerceptualHashFromImage(src image.Image) uint64 {
	// Resize to 9 wide x 8 tall (9 columns so we get 8 column diffs per row).
	resized := image.NewGray(image.Rect(0, 0, 9, 8))
	draw.CatmullRom.Scale(resized, resized.Bounds(), src, src.Bounds(), draw.Over, nil)

	var hash uint64
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			left := resized.GrayAt(x, y).Y
			right := resized.GrayAt(x+1, y).Y
			if left > right {
				hash |= 1 << uint(y*8+x)
			}
		}
	}
	return hash
}

// HammingDistance returns the number of differing bits between two hashes.
func HammingDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// Similarity returns a similarity score between 0.0 and 1.0 for two hashes.
// 1.0 means identical, 0.0 means completely different.
func Similarity(a, b uint64) float64 {
	distance := HammingDistance(a, b)
	return 1.0 - float64(distance)/64.0
}

// HashHex formats a perceptual hash as a zero-padded 16-character hex string.
func HashHex(h uint64) string {
	return fmt.Sprintf("%016x", h)
}

// ParseHashHex parses a hex-encoded perceptual hash string.
// Returns an error if the string is not a valid 64-bit hex value.
func ParseHashHex(s string) (uint64, error) {
	return strconv.ParseUint(s, 16, 64)
}

// ContentHash returns the SHA-256 of the exact bytes as a lowercase hex
// string. Unlike PerceptualHash it does not decode the image: it answers
// only "are these two byte sequences identical", which is the one duplicate
// claim that has no false positives. It is the cheap first tier of duplicate
// detection; PerceptualHash is the expensive second tier that additionally
// catches re-encoded or metadata-stripped copies of the same picture.
func ContentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ErrImageTooLarge reports that a file exceeded the read bound and was not
// read into memory. It is a sentinel so callers CAN distinguish "too big to
// be worth reading" from "unreadable" (a real I/O fault) -- a distinction
// that now has two different consumers: the background hashing callers
// log-and-skip both cases identically, while the logo-trim REQUEST handler
// maps this case to HTTP 413 and a genuine I/O fault to 500. The bound is
// MaxDecodeBytes -- the same constant decodeWithLimit enforces, deliberately
// reused rather than duplicated so the read bound and the decode bound cannot
// drift apart.
//
// The wording is deliberately free of "hash": this error is no longer
// hashing-specific, and a message naming the wrong operation would be
// actively misleading in the trim handler's logs.
var ErrImageTooLarge = errors.New("image file too large")

// FileHashes carries the hashes computed from a single read of one image file.
// Perceptual is zero when the caller did not ask for it (see HashFile), which
// is indistinguishable from a decode that legitimately produced the all-zero
// hash; callers that need the perceptual hash must request it explicitly and
// treat zero as "unusable" exactly as the stored-phash path does.
type FileHashes struct {
	Content    string
	Perceptual uint64
}

// HashFile reads path exactly once and returns its hashes.
//
// The content hash is always computed: it is a SHA-256 over bytes already in
// memory and costs nothing next to the read itself. The perceptual hash is
// computed only when needPerceptual is true, because that requires a full
// image decode and resample -- by far the most expensive step, and pure waste
// for a file whose hash is already persisted or that an exact-duplicate match
// has already accounted for.
//
// This single-read shape is the point: duplicate detection needs both tiers,
// and reading the file twice (once per tier) would give back most of what
// ordering the cheap tier first buys.
//
// The security boundary on path is enforced at the call sites, exactly as for
// ReadProvenance: callers construct paths from trusted sources (database rows,
// filesystem discovery, fixed naming patterns), never from request input.
//
// The read is bounded at MaxDecodeBytes. That bound is a hard requirement, not
// a nicety: the path is trusted but its CONTENTS are not sized by us -- these
// are operator-supplied library directories, and an arbitrarily large file
// sitting in one used to be read whole into memory here. Go has no
// allocation-failure path, so an over-budget allocation is a fatal runtime
// error rather than an error value; under a container memory limit that is a
// SIGKILL and a restart loop, not something a caller can recover from.
//
// Files over the bound return ErrImageTooLarge; the read never allocates
// more than MaxDecodeBytes+1 (26,214,401 bytes) regardless of file size --
// that fixed cap, not "zero allocation" for an oversized file, is the actual
// guarantee. The realized peak is higher still: io.ReadAll grows its buffer
// by repeated append, and append's growslice keeps the old backing array
// alive while it copies into the new, larger one, so the transient
// high-water mark is roughly 2x the bound -- about 50-60 MB for today's
// 25 MB limit. Size any container memory budget off that peak, not the
// nominal limit.
func HashFile(path string, needPerceptual bool) (FileHashes, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return FileHashes{}, fmt.Errorf("reading image for hashing: %w", err)
	}
	defer func() { _ = f.Close() }()

	// io.LimitReader, not os.Stat: a Stat-then-read has a TOCTOU window in
	// which the file can grow between the size check and the read, so the
	// check bounds a number while the read stays unbounded. The LimitReader
	// bounds the ALLOCATION itself, which is the thing that has to be
	// bounded. Reading one byte past the limit is what distinguishes
	// "exactly at the limit" from "over it".
	data, err := io.ReadAll(io.LimitReader(f, MaxDecodeBytes+1))
	if err != nil {
		return FileHashes{}, fmt.Errorf("reading image for hashing: %w", err)
	}
	if int64(len(data)) > MaxDecodeBytes {
		return FileHashes{}, fmt.Errorf("hashing %s: %w (max %d bytes)", path, ErrImageTooLarge, MaxDecodeBytes)
	}

	h := FileHashes{Content: ContentHash(data)}
	if !needPerceptual {
		return h, nil
	}

	perceptual, err := PerceptualHash(bytes.NewReader(data))
	if err != nil {
		// The content hash is still valid and useful on its own (an
		// undecodable file can still be byte-compared), so return it
		// alongside the error rather than discarding it.
		return h, fmt.Errorf("perceptual hash for %s: %w", path, err)
	}
	h.Perceptual = perceptual
	return h, nil
}
