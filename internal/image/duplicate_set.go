package image

// DefaultDuplicateTolerance is the perceptual similarity (see Similarity) at or
// above which two images are treated as the same picture for duplicate
// detection. Similarity = 1 - Hamming/64, so 0.90 admits a match within 6
// differing bits of a 64-bit dHash: the near-duplicate band of re-encodes,
// rescales and requantizations of one photograph. Two different photographs,
// even of one subject in one session, typically land 20+ bits apart.
//
// It lives here rather than in the rule package so the local duplicate rule
// and the platform backdrop prune (#3138) share ONE number with ONE meaning;
// a second copy would be free to drift.
const DefaultDuplicateTolerance = 0.90

// RepresentativeDeletionSet returns the members of order that can be removed
// as duplicates without losing any distinct picture, given a pairwise
// similarity predicate. Each removable member maps to the representative that
// absorbed it -- the copy kept in its place -- so a caller that must show or
// re-verify the survivor does not have to re-derive it.
//
// Perceptual similarity is pairwise, not transitive: sim(a,b) and sim(b,c) can
// both clear tolerance while sim(a,c) does not, so a and c hold distinct
// artwork even though b resembles both. Grouping naively (union-find, or
// "delete the higher member of every pair") would treat a..c as one group and
// destroy c.
//
// The walk visits order front to back. Each member not already marked for
// deletion becomes a REPRESENTATIVE and survives. A later member is marked for
// deletion only when paired(rep, member) holds DIRECTLY with a surviving
// representative -- never through a chain via a member that is itself already
// marked. In the a~b, b~c, a!~c case: a is a representative and deletes b; c is
// never directly paired with a, so it survives as its own representative.
//
// The ORDER is the survivor policy: earlier members are preferred as
// representatives. The local fanart rule passes ascending slot order (lowest
// slot survives); the platform prune passes its quality ranking. paired is
// called only with rep earlier than member in order.
func RepresentativeDeletionSet(order []int, paired func(rep, member int) bool) map[int]int {
	toDelete := make(map[int]int)
	for i, rep := range order {
		if _, gone := toDelete[rep]; gone {
			continue
		}
		for _, member := range order[i+1:] {
			if _, gone := toDelete[member]; gone {
				continue
			}
			if paired(rep, member) {
				toDelete[member] = rep
			}
		}
	}
	return toDelete
}
