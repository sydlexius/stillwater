// This file collects the four image write/delete methods that are
// byte-for-byte identical between the Emby and Jellyfin REST surfaces:
// UploadImage, UploadImageAtIndex, DeleteImage, and DeleteImageAtIndex.
// Each per-platform client.go/push.go keeps a thin method that maps its own
// Stillwater image-type string to the platform's image-type string (via its
// per-package mapImageType, which stays per-package -- Emby and Jellyfin use
// different lookup tables, matching the precedent set by GetArtistImageRaw
// in library_getters.go) and then delegates its body here.
package mediabrowser

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sydlexius/stillwater/internal/connection/httpclient"
)

// AuthErrorClassifier wraps an error that may carry an auth-class
// (401/403) httpclient.StatusError with the caller's platform-specific
// sentinel (emby.ErrAuthRequired / jellyfin.ErrAuthRequired). Callers pass
// their package's wrapAuthIfStatusAuth (itself a thin binding of
// ClassifyAuthError) so the shared functions below produce exactly the same
// sentinel-wrapped errors the hand-rolled per-package methods did.
type AuthErrorClassifier func(error) error

// doImageRequest executes the shared Do -> status-check -> classify ->
// drain shape common to all four image write/delete operations below. It
// does not log; each caller logs on success with its own message and
// fields, since the log content varies per operation (upload vs delete,
// with/without index). opDesc must match the caller's exact operation name
// so the wrapped error strings stay byte-identical to the pre-refactor
// per-function implementations ("executing image upload", "image upload
// failed with status %d", etc.).
func doImageRequest(ctx context.Context, t Transport, method, path string, body io.Reader, contentType, opDesc string, classifyAuth AuthErrorClassifier) error {
	resp, err := t.Do(ctx, method, path, body, contentType)
	if err != nil {
		return fmt.Errorf("executing %s: %w", opDesc, err)
	}
	defer resp.Body.Close() //nolint:errcheck // Close error not actionable on HTTP response cleanup

	if resp.StatusCode >= 300 {
		statusErr := httpclient.ReadBoundedStatusError(resp)
		formatted := fmt.Errorf("%s failed with status %d: %s", opDesc, statusErr.StatusCode, statusErr.Body)
		return classifyAuth(errors.Join(formatted, statusErr))
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// UploadImageRaw uploads image bytes to the peer for the given artist and
// platform-mapped image type. POST /Items/{artistID}/Images/{platformType}.
// Callers map their own Stillwater image-type string (thumb, fanart, logo,
// banner) to the platform's image-type string via their per-package
// mapImageType before calling this -- see the file comment. An empty
// platformType signals an unmapped Stillwater type; the caller's original
// imageType is passed through only for the error message.
//
// Identical on Emby and Jellyfin: both expect the image body as
// base64-encoded plain text, with Content-Type declaring the image format
// so the peer knows what to decode it as after base64.
func UploadImageRaw(ctx context.Context, t Transport, logger *slog.Logger, platform, artistID, platformType, requestedImageType string, data []byte, contentType string, classifyAuth AuthErrorClassifier) error {
	if platformType == "" {
		return fmt.Errorf("unsupported image type: %s", requestedImageType)
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	path := fmt.Sprintf("/Items/%s/Images/%s", artistID, platformType)

	if err := doImageRequest(ctx, t, http.MethodPost, path, strings.NewReader(encoded), contentType, "image upload", classifyAuth); err != nil {
		return err
	}

	if logger != nil {
		logger.Debug("image uploaded", "platform", platform, "artist_id", artistID, "type", platformType)
	}
	return nil
}

// UploadImageAtIndexRaw uploads image bytes at a specific index (used for
// backdrops, where the peer supports multiple images per artist).
// POST /Items/{artistID}/Images/{platformType}/{index}. See UploadImageRaw
// for the platformType-mapping contract.
func UploadImageAtIndexRaw(ctx context.Context, t Transport, logger *slog.Logger, platform, artistID, platformType, requestedImageType string, index int, data []byte, contentType string, classifyAuth AuthErrorClassifier) error {
	if index < 0 {
		return fmt.Errorf("invalid image index: %d", index)
	}
	if platformType == "" {
		return fmt.Errorf("unsupported image type: %s", requestedImageType)
	}

	encoded := base64.StdEncoding.EncodeToString(data)
	path := fmt.Sprintf("/Items/%s/Images/%s/%d", artistID, platformType, index)

	if err := doImageRequest(ctx, t, http.MethodPost, path, strings.NewReader(encoded), contentType, "indexed image upload", classifyAuth); err != nil {
		return err
	}

	if logger != nil {
		logger.Debug("image uploaded at index", "platform", platform, "artist_id", artistID, "type", platformType, "index", index)
	}
	return nil
}

// DeleteImageRaw deletes an image from the peer for the given artist and
// platform-mapped image type. DELETE /Items/{artistID}/Images/{platformType}.
// See UploadImageRaw for the platformType-mapping contract.
func DeleteImageRaw(ctx context.Context, t Transport, logger *slog.Logger, platform, artistID, platformType, requestedImageType string, classifyAuth AuthErrorClassifier) error {
	if platformType == "" {
		return fmt.Errorf("unsupported image type: %s", requestedImageType)
	}

	path := fmt.Sprintf("/Items/%s/Images/%s", artistID, platformType)

	if err := doImageRequest(ctx, t, http.MethodDelete, path, nil, "", "image delete", classifyAuth); err != nil {
		return err
	}

	if logger != nil {
		logger.Debug("image deleted", "platform", platform, "artist_id", artistID, "type", platformType)
	}
	return nil
}

// DeleteImageAtIndexRaw deletes the image at a specific index for the given
// artist. DELETE /Items/{artistID}/Images/{platformType}/{index}. Used to
// prune redundant backdrops on the platform (#2540 remote prune). The peer
// re-indexes remaining backdrops after a delete, so callers pruning
// multiple indices MUST delete high-index-first. See UploadImageRaw for the
// platformType-mapping contract.
func DeleteImageAtIndexRaw(ctx context.Context, t Transport, logger *slog.Logger, platform, artistID, platformType, requestedImageType string, index int, classifyAuth AuthErrorClassifier) error {
	if index < 0 {
		return fmt.Errorf("invalid image index: %d", index)
	}
	if platformType == "" {
		return fmt.Errorf("unsupported image type: %s", requestedImageType)
	}

	path := fmt.Sprintf("/Items/%s/Images/%s/%d", artistID, platformType, index)

	if err := doImageRequest(ctx, t, http.MethodDelete, path, nil, "", "indexed image delete", classifyAuth); err != nil {
		return err
	}

	if logger != nil {
		logger.Debug("image deleted at index", "platform", platform, "artist_id", artistID, "type", platformType, "index", index)
	}
	return nil
}
