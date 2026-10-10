// Package search implements hybrid search result merging.
package search

import (
	"slices"
	"strings"

	"github.com/kbsearch/backend-go/internal/domain"
)

// RRFK is the RRF rank constant.
const RRFK = 60

// MaxCandidates caps per-branch candidate fetch.
const MaxCandidates = 100

// ScoredCandidate is a candidate with its merged score.
type ScoredCandidate struct {
	Candidate domain.SearchCandidate
	Score     float64
}

func rankByChunkID(items []domain.SearchCandidate) map[string]int {
	ranks := make(map[string]int, len(items))
	for i, item := range items {
		ranks[item.ChunkID] = i + 1
	}
	return ranks
}

// RRFMerge merges lexical and vector branches by chunk_id.
// Ties break by chunk_id for a deterministic order.
func RRFMerge(lexical, vector []domain.SearchCandidate) []ScoredCandidate {
	lexicalRanks := rankByChunkID(lexical)
	vectorRanks := rankByChunkID(vector)

	byID := make(map[string]domain.SearchCandidate, len(lexical)+len(vector))
	for _, branch := range [][]domain.SearchCandidate{lexical, vector} {
		for _, item := range branch {
			if _, seen := byID[item.ChunkID]; !seen {
				byID[item.ChunkID] = item
			}
		}
	}

	result := make([]ScoredCandidate, 0, len(byID))
	for chunkID, candidate := range byID {
		result = append(result, ScoredCandidate{
			Candidate: candidate,
			Score:     mergedScore(lexicalRanks, vectorRanks, chunkID),
		})
	}

	slices.SortFunc(result, func(a, b ScoredCandidate) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Candidate.ChunkID, b.Candidate.ChunkID)
	})
	return result
}

// mergedScore scores one chunk; a branch that lacks it contributes 0.
func mergedScore(lexicalRanks, vectorRanks map[string]int, chunkID string) float64 {
	var score float64
	if rank, ok := lexicalRanks[chunkID]; ok {
		score += 1.0 / float64(RRFK+rank)
	}
	if rank, ok := vectorRanks[chunkID]; ok {
		score += 1.0 / float64(RRFK+rank)
	}
	return score
}

// CandidateLimit caps per-branch fetch at min(100, 5*top_k).
func CandidateLimit(topK int) int {
	limit := 5 * topK
	if limit > MaxCandidates {
		return MaxCandidates
	}
	return limit
}
