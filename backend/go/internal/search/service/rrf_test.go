package search_test

import (
	"fmt"
	"testing"

	"github.com/kbsearch/kb/internal/envelope"
	"github.com/kbsearch/kb/internal/search/service"
)

// Кейсы перенесены из backend/tests/test_search.py:131-183 один в один.
// Смысл: критерий приёмки 7.6 требует, чтобы RRF давал воспроизводимый
// порядок на заданных mock-списках. Список кандидатов и ожидаемые score
// скопированы, чтобы расхождение означало именно расхождение алгоритма.

// candidate повторяет helper candidate() из test_search.py:112.
func candidate(number int, score float64) domain.SearchCandidate {
	return domain.SearchCandidate{
		ChunkID:            fmt.Sprintf("sha256:%s%d", repeat("c", 63), number),
		TenantID:           "11111111-1111-4111-8111-111111111111",
		KBID:               "22222222-2222-4222-8222-222222222222",
		DocumentID:         "33333333-3333-4333-8333-333333333333",
		VersionID:          "44444444-4444-4444-8444-444444444444",
		ChunkNumber:        number,
		Title:              "Инструкция по выгрузке",
		Text:               fmt.Sprintf("Текст чанка номер %d", number),
		EmbeddingProfileID: "sha256:" + repeat("b", 64),
		Score:              score,
	}
}

func repeat(c string, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, c[0])
	}
	return string(out)
}

func scoresOf(merged []search.ScoredCandidate) map[string]float64 {
	out := make(map[string]float64, len(merged))
	for _, item := range merged {
		out[item.Candidate.ChunkID] = item.Score
	}
	return out
}

func idsOf(merged []search.ScoredCandidate) []string {
	out := make([]string, 0, len(merged))
	for _, item := range merged {
		out = append(out, item.Candidate.ChunkID)
	}
	return out
}

// TestRRFSumsReciprocalRanks повторяет test_rrf_sums_reciprocal_ranks.
func TestRRFSumsReciprocalRanks(t *testing.T) {
	merged := search.RRFMerge([]domain.SearchCandidate{candidate(1, 1)}, []domain.SearchCandidate{candidate(2, 1)})
	scores := scoresOf(merged)

	want := 1.0 / 61
	if diff := scores[candidate(1, 1).ChunkID] - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("score чанка 1 = %v, ожидалось %v", scores[candidate(1, 1).ChunkID], want)
	}
	if diff := scores[candidate(2, 1).ChunkID] - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("score чанка 2 = %v, ожидалось %v", scores[candidate(2, 1).ChunkID], want)
	}

	// Порядок при равном score детерминирован: по chunk_id. Без явного
	// SecondaryKey сортировка Go для равных элементов нестабильна.
	assertSortedByScoreThenID(t, merged)
}

// TestRRFBothBranchesOutranksSingleBranch повторяет
// test_rrf_both_branches_outranks_single_branch.
func TestRRFBothBranchesOutranksSingleBranch(t *testing.T) {
	both := candidate(3, 1)
	lexical := []domain.SearchCandidate{candidate(1, 1), candidate(2, 1), both}
	vector := []domain.SearchCandidate{candidate(4, 1), both, candidate(5, 1)}

	merged := search.RRFMerge(lexical, vector)
	scores := scoresOf(merged)

	// В lexical ранг 3, в vector ранг 2: 1/(60+3) + 1/(60+2).
	wantBoth := 1.0/63 + 1.0/62
	if diff := scores[both.ChunkID] - wantBoth; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("score чанка в обеих ветвях = %v, ожидалось %v", scores[both.ChunkID], wantBoth)
	}
	if merged[0].Candidate.ChunkID != both.ChunkID {
		t.Errorf("первым идёт %s, ожидался чанк %s", merged[0].Candidate.ChunkID, both.ChunkID)
	}
	maxScore := 0.0
	for _, score := range scores {
		if score > maxScore {
			maxScore = score
		}
	}
	if diff := maxScore - wantBoth; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("максимальный score = %v, ожидалось %v", maxScore, wantBoth)
	}
}

// TestRRFMissingBranchCountsAsZero повторяет
// test_rrf_missing_branch_counts_as_zero.
func TestRRFMissingBranchCountsAsZero(t *testing.T) {
	merged := search.RRFMerge([]domain.SearchCandidate{candidate(7, 1)}, nil)
	if len(merged) != 1 {
		t.Fatalf("кандидатов %d, ожидался 1", len(merged))
	}
	want := 1.0 / 61
	if diff := merged[0].Score - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("score = %v, ожидалось %v", merged[0].Score, want)
	}
}

// TestRRFIsReproducibleOnFixedCandidates повторяет
// test_rrf_is_reproducible_on_fixed_candidates.
func TestRRFIsReproducibleOnFixedCandidates(t *testing.T) {
	lexical := []domain.SearchCandidate{candidate(1, 1), candidate(2, 1), candidate(3, 1), candidate(4, 1)}
	vector := []domain.SearchCandidate{candidate(4, 1), candidate(3, 1), candidate(2, 1), candidate(1, 1)}

	first := idsOf(search.RRFMerge(lexical, vector))
	for i := 0; i < 5; i++ {
		if got := idsOf(search.RRFMerge(lexical, vector)); !equal(got, first) {
			t.Fatalf("порядок изменился на повторе %d:\n получено %v\n ожидалось %v", i, got, first)
		}
	}
	// Перевёрнутые ветви дают тот же порядок: RRF симметричен по рангам.
	swapped := idsOf(search.RRFMerge(vector, lexical))
	if !equal(swapped, first) {
		t.Errorf("перевёрнутые ветви дали другой порядок:\n получено %v\n ожидалось %v", swapped, first)
	}
}

// TestRRFRanksStartFromOne повторяет test_rrf_ranks_start_from_one:
// первый кандидат ветви даёт 1/(60+1), а не 1/60.
func TestRRFRanksStartFromOne(t *testing.T) {
	merged := search.RRFMerge([]domain.SearchCandidate{candidate(1, 1), candidate(2, 1)}, nil)
	want := 1.0 / 61
	if diff := merged[0].Score - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("score первого = %v, ожидалось %v (ранги должны начинаться с 1)", merged[0].Score, want)
	}
}

// TestRRFTakesCandidateFromFirstBranch: поля кандидата берутся из первой
// ветви, где он встретился. Ветви возвращают разный score, и подставлять
// score второй ветви нельзя — он не попадает в выдачу.
func TestRRFTakesCandidateFromFirstBranch(t *testing.T) {
	lexical := domain.SearchCandidate{ChunkID: "chunk-1", Text: "из lexical", Score: 7}
	vector := domain.SearchCandidate{ChunkID: "chunk-1", Text: "из vector", Score: 0.1}

	merged := search.RRFMerge([]domain.SearchCandidate{lexical}, []domain.SearchCandidate{vector})
	if len(merged) != 1 {
		t.Fatalf("кандидатов %d, ожидался 1", len(merged))
	}
	if merged[0].Candidate.Text != "из lexical" {
		t.Errorf("Text = %q, ожидалось %q", merged[0].Candidate.Text, "из lexical")
	}
	if merged[0].Candidate.Score != 7 {
		t.Errorf("Score = %v, ожидалось 7", merged[0].Candidate.Score)
	}
}

// TestCandidateLimit проверяет min(100, 5 * top_k) из ТЗ 7.4 п.5.
func TestCandidateLimit(t *testing.T) {
	cases := []struct {
		topK int
		want int
	}{
		{1, 5},
		{5, 25},
		{20, 100},
		// Потолок 100: при top_k = 50 без него ветви вернули бы по 250.
		{50, 100},
		{100, 100},
	}
	for _, tc := range cases {
		if got := search.CandidateLimit(tc.topK); got != tc.want {
			t.Errorf("CandidateLimit(%d) = %d, ожидалось %d", tc.topK, got, tc.want)
		}
	}
}

func TestRRFOnEmptyBranches(t *testing.T) {
	if merged := search.RRFMerge(nil, nil); len(merged) != 0 {
		t.Errorf("RRFMerge(nil, nil) вернул %d кандидатов", len(merged))
	}
}

// assertSortedByScoreThenID проверяет инвариант порядка: score по убыванию,
// при равенстве — по chunk_id по возрастанию.
func assertSortedByScoreThenID(t *testing.T, merged []search.ScoredCandidate) {
	t.Helper()
	for i := 1; i < len(merged); i++ {
		prev, cur := merged[i-1], merged[i]
		switch {
		case prev.Score > cur.Score+1e-12:
			t.Errorf("позиция %d: score %v идёт после меньшего %v", i, prev.Score, cur.Score)
		case prev.Score < cur.Score-1e-12:
			t.Errorf("позиция %d: score %v идёт после большего %v", i, prev.Score, cur.Score)
		case prev.Candidate.ChunkID > cur.Candidate.ChunkID:
			t.Errorf("при равном score %v идёт раньше %v", prev.Candidate.ChunkID, cur.Candidate.ChunkID)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
