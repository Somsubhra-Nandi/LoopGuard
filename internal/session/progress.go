package session

// ProgressTracker keeps the progress score of recent calls (LLD §5.1). Each
// entry also keeps the family-relevant part of the score, so detectors and
// the policy can ask how much progress one family made.
type ProgressTracker struct {
	scores *Ring[scored]
}

type scored struct {
	seq         uint64
	score       float64
	family      string
	familyScore float64
}

// NewProgressTracker returns a tracker that remembers capacity calls.
func NewProgressTracker(capacity int) *ProgressTracker {
	return &ProgressTracker{scores: NewRing[scored](capacity)}
}

// Add records the score of the call with sequence number seq, with no
// family-relevant part.
func (p *ProgressTracker) Add(seq uint64, score float64) {
	p.scores.Push(scored{seq: seq, score: score})
}

// Record records the score of the call with sequence number seq, and the
// part of it that is progress for family.
func (p *ProgressTracker) Record(seq uint64, score float64, family string, familyScore float64) {
	p.scores.Push(scored{seq: seq, score: score, family: family, familyScore: familyScore})
}

// Recent returns the sum of the scores of the last n calls.
func (p *ProgressTracker) Recent(n int) float64 {
	sum := 0.0
	for i := p.scores.Len() - 1; i >= 0 && n > 0; i, n = i-1, n-1 {
		sum += p.scores.At(i).score
	}
	return sum
}

// Since returns the sum of the scores of calls after seq (exclusive).
func (p *ProgressTracker) Since(seq uint64) float64 {
	sum := 0.0
	for i := p.scores.Len() - 1; i >= 0; i-- {
		s := p.scores.At(i)
		if s.seq <= seq {
			break
		}
		sum += s.score
	}
	return sum
}

// FamilySince returns the family-relevant progress of family made by calls
// after seq (exclusive): the family recovering, its error changing to a
// never-seen signature, or its failing-test count dropping.
func (p *ProgressTracker) FamilySince(family string, seq uint64) float64 {
	sum := 0.0
	for i := p.scores.Len() - 1; i >= 0; i-- {
		s := p.scores.At(i)
		if s.seq <= seq {
			break
		}
		if s.family == family {
			sum += s.familyScore
		}
	}
	return sum
}
