package search

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRRFMergesAndRanksByScore(t *testing.T) {
	// "a" есть в обеих ранжировках на первом месте — должен выиграть.
	// "b" только в первой на втором, "c" только во второй на втором.
	rankings := [][]string{{"a", "b"}, {"a", "c"}}
	got := RRF(rankings, 10)

	if len(got) != 3 {
		t.Fatalf("ожидалось 3 хита, получено %d", len(got))
	}
	if got[0].ID != "a" {
		t.Errorf("ожидался лидер \"a\", получен %q", got[0].ID)
	}
	// b и c набрали одинаково (одно вхождение на позиции 2) — tie-break по первому появлению.
	if got[1].ID != "b" || got[2].ID != "c" {
		t.Errorf("tie-break сломан: ожидались [b c], получены [%s %s]", got[1].ID, got[2].ID)
	}
}

func TestRRFLimitTruncates(t *testing.T) {
	got := RRF([][]string{{"a", "b", "c"}}, 2)
	if len(got) != 2 {
		t.Fatalf("limit не обрезал: %d", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("порядок нарушен: [%s %s]", got[0].ID, got[1].ID)
	}
}

func TestRRFEmptyRankings(t *testing.T) {
	if got := RRF(nil, 5); len(got) != 0 {
		t.Errorf("пустые ранжировки должны давать пустой результат, получено %d", len(got))
	}
}

func TestRankingsSkipsEmpty(t *testing.T) {
	got := Rankings([]string{}, []string{"a"}, nil, []string{"b"})
	if len(got) != 2 {
		t.Fatalf("пустые ранжировки должны отбрасываться: %d", len(got))
	}
}

func TestSizeGrowsWithTopK(t *testing.T) {
	if Size(5) != 15 {
		t.Errorf("Size(5) = %d, ожидалось 15", Size(5))
	}
	// Минимум 10 кандидатов даже на top_k=1.
	if Size(1) != 10 {
		t.Errorf("Size(1) = %d, ожидалось 10", Size(1))
	}
}

func TestToHitBuildsDownloadURL(t *testing.T) {
	src := map[string]any{
		"chunk_id":     "c1",
		"document_id":  "d1",
		"document_ref": "8f14e45f-ceea-467f-a1b2-3c4d5e6f7081",
		"version_id":   "v1",
		"title":        "T",
		"text":         "X",
		"section_path": []any{"A", "B"},
		"page_start":   float64(3),
	}
	hit := ToHit(src, 0.5)
	if hit.DownloadURL != "/api/v1/documents/d1/source" {
		t.Errorf("download_url = %q", hit.DownloadURL)
	}
	if hit.Locator.PageStart == nil || *hit.Locator.PageStart != 3 {
		t.Errorf("page_start не разобран: %v", hit.Locator.PageStart)
	}
	if len(hit.Locator.SectionPath) != 2 {
		t.Errorf("section_path = %v", hit.Locator.SectionPath)
	}
	if hit.DocumentRef != "8f14e45f-ceea-467f-a1b2-3c4d5e6f7081" {
		t.Errorf("document_ref = %q", hit.DocumentRef)
	}
}

func TestScopeFilterUsesIndexFields(t *testing.T) {
	// Имена полей в фильтре должны совпадать с маппингом ingest: расхождение
	// не даёт ошибки, а молча исключает все документы из выдачи.
	raw, err := json.Marshal(LexicalBody(Scope{Channels: []string{"public"}}, "q", 5, ""))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, `"open_to_all":true`) {
		t.Errorf("фильтр не использует open_to_all: %s", body)
	}
	if strings.Contains(body, `"open":true`) {
		t.Errorf("в фильтре осталось несуществующее поле open: %s", body)
	}
	if !strings.Contains(body, `"group_keys":[]`) {
		t.Errorf("nil-группы должны сериализоваться в пустой массив: %s", body)
	}
}

func TestFocusBoostsWithoutFiltering(t *testing.T) {
	// Привязка страницы — предпочтение: документ получает boost, но остальные
	// не исключаются, иначе подсказка клиента стала бы фильтром доступа.
	raw, err := json.Marshal(
		LexicalBody(Scope{Channels: []string{"public"}}, "q", 5, "doc-ref"),
	)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, `"document_ref":{"boost":3,"value":"doc-ref"}`) {
		t.Errorf("нет повышения для привязанного документа: %s", body)
	}
	if strings.Contains(body, `"filter":[{"term":{"document_ref"`) {
		t.Errorf("привязка попала в фильтр: %s", body)
	}
}
