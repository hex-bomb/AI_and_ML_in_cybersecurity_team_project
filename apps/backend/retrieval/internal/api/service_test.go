package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"vektor/retrieval/internal/search"
)

// esStub отвечает на _search: kNN и лексику различаем по телу запроса.
func esStub(t *testing.T, fail bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("тело запроса: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id, title := "lex", "Лексика"
		if _, ok := body["knn"]; ok {
			id, title = "vec", "Вектор"
		}
		_, _ = io.WriteString(w, `{"hits":{"hits":[{"_id":"`+id+`","_source":{"document_id":"`+id+`","title":"`+title+`","text":"t"}}]}}`)
	}))
}

func embeddingStub(t *testing.T, fail bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"vectors":[[0.1,0.2,0.3]]}`)
	}))
}

func service(t *testing.T, esFail, embedFail bool) *HybridSearch {
	t.Helper()
	es := esStub(t, esFail)
	embedding := embeddingStub(t, embedFail)
	t.Cleanup(es.Close)
	t.Cleanup(embedding.Close)
	return NewHybridSearch(
		search.NewES(es.URL, "idx", es.Client()),
		search.NewEmbedder(embedding.URL, embedding.Client()),
	)
}

func scope() search.Scope {
	return search.Scope{Channels: []string{"public"}, Groups: []string{}}
}

func TestSearchFallsBackToLexicalWhenEmbeddingFails(t *testing.T) {
	hits, err := service(t, false, true).Search(context.Background(), scope(), "гарантия", 3)
	if err != nil {
		t.Fatalf("отказ embedding не должен быть ошибкой поиска: %v", err)
	}
	if len(hits) != 1 || hits[0].DocumentID != "lex" {
		t.Fatalf("ожидался только лексический хит, получено: %+v", hits)
	}
}

func TestSearchFailsWhenBothWaysAreDown(t *testing.T) {
	hits, err := service(t, false, true).Search(context.Background(), scope(), "гарантия", 3)
	if err != nil || len(hits) == 0 {
		t.Fatalf("лексика жива, отказ embedding не должен валить запрос: hits=%+v err=%v", hits, err)
	}

	if _, err := service(t, true, false).Search(context.Background(), scope(), "гарантия", 3); err == nil {
		t.Fatal("ES недоступен — недоступны обе ветки, нужна ошибка")
	}
	if _, err := service(t, true, true).Search(context.Background(), scope(), "гарантия", 3); err == nil {
		t.Fatal("при отказе обеих веток поиска нужна ошибка")
	}
}

func TestSearchFusesBothRankings(t *testing.T) {
	hits, err := service(t, false, false).Search(context.Background(), scope(), "гарантия", 3)
	if err != nil {
		t.Fatalf("неожиданная ошибка: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("ожидалось слияние двух ранжировок, получено: %+v", hits)
	}
}
