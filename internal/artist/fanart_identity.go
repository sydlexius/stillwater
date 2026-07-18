package artist

// fanart_identity.go -- the registry-loader half of #2540's shared
// phash-identity foundation (internal/image/identity.go is the comparison
// primitive half). Lives in this package, not internal/rule, because it
// reads artist_images directly and both internal/rule and internal/publish
// already import internal/artist.
//
// No caching here: the write-time guard (#2565) and outbound gate (#2566)
// each cache the built index once per scope (once per write, once per sync
// call) -- see design-2540.md section 4. Rebuilding here on every call keeps
// this loader a simple, cheap, always-fresh read.

import (
	"context"
	"fmt"

	"github.com/sydlexius/stillwater/internal/image"
)

// BuildFanartIdentityIndex loads the cross-artist fanart comparison registry:
// one entry per exists_flag=1 fanart row in the WHOLE library that carries a
// usable perceptual hash.
//
// "Usable" mirrors usablePHash (internal/rule/phash_mismatch.go:267-279)
// exactly: an empty phash column ("never hashed") and an unparsable hex
// string are both skipped, and so is the parsed-zero value -- a stored
// 0000000000000000 is indistinguishable from "never hashed" and admitting it
// would manufacture a perfect collision between every pair of unhashed
// images in the library, exactly the trap usablePHash exists to avoid.
//
// FANART ONLY, deliberately not thumb. The #2564 detector also loads thumb
// rows, but only as corroborating attribution on an already-raised suspect
// (phash_mismatch.go:41-44, 436-453) -- it never uses a thumb collision to
// raise one. This loader feeds the write/push GUARD registry (design-2540.md
// section 1 and section 4 item 2), which needs only the signal that can
// actually raise a finding: cross-artist fanart-to-fanart collision. A thumb
// (portrait headshot) does not perceptually collide with a legitimate
// backdrop (wide promo shot) of the same person -- they are different
// photographs 20-30+ bits apart -- so including thumb rows here would add
// registry-build cost for a signal these guards never act on.
func (s *Service) BuildFanartIdentityIndex(ctx context.Context) ([]image.FanartIdentityEntry, error) {
	rows, err := s.images.AllFanartHashes(ctx)
	if err != nil {
		return nil, fmt.Errorf("building fanart identity index: %w", err)
	}

	entries := make([]image.FanartIdentityEntry, 0, len(rows))
	for _, r := range rows {
		h, ok := usableFanartHash(r.PHashHex)
		if !ok {
			continue
		}
		entries = append(entries, image.FanartIdentityEntry{ArtistID: r.ArtistID, PHash: h})
	}
	return entries, nil
}

// usableFanartHash parses a stored phash and reports whether it can be
// compared, mirroring internal/rule/phash_mismatch.go's usablePHash: an
// empty column means never-hashed, an unparsable string is corrupt data, and
// a zero hash is treated identically to "never hashed" because it cannot be
// told apart from it and admitting it would manufacture false collisions
// against every other unhashed image.
func usableFanartHash(hex string) (uint64, bool) {
	if hex == "" {
		return 0, false
	}
	h, err := image.ParseHashHex(hex)
	if err != nil {
		return 0, false
	}
	if h == 0 {
		return 0, false
	}
	return h, true
}
