package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ES — клиент поискового индекса Elasticsearch.
type ES struct {
	url   string
	index string
	http  *http.Client
}

func NewES(url, index string, client *http.Client) *ES {
	return &ES{url: url, index: index, http: client}
}

// SearchResult — источники документов и порядок по релевантности.
type SearchResult struct {
	Sources map[string]map[string]any
	Ranking []string
}

// Search выполняет запрос к индексу и возвращает источники + ранжировку.
func (e *ES) Search(ctx context.Context, body map[string]any) (SearchResult, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return SearchResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/"+e.index+"/_search", bytes.NewReader(raw))
	if err != nil {
		return SearchResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.http.Do(req)
	if err != nil {
		return SearchResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return SearchResult{}, fmt.Errorf("elasticsearch status %d", resp.StatusCode)
	}

	var out struct {
		Hits struct {
			Hits []struct {
				ID     string         `json:"_id"`
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return SearchResult{}, err
	}

	result := SearchResult{
		Sources: make(map[string]map[string]any, len(out.Hits.Hits)),
		Ranking: make([]string, 0, len(out.Hits.Hits)),
	}
	for _, h := range out.Hits.Hits {
		result.Sources[h.ID] = h.Source
		result.Ranking = append(result.Ranking, h.ID)
	}
	return result, nil
}

// Embedder — клиент сервиса эмбеддингов.
type Embedder struct {
	url  string
	http *http.Client
}

func NewEmbedder(url string, client *http.Client) *Embedder {
	return &Embedder{url: url, http: client}
}

// Embed превращает текст в вектор. kind=query сообщает сервису, что кодируется
// поисковый запрос, а не документ.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float64, error) {
	body, err := json.Marshal(map[string]any{"texts": []string{text}, "kind": "query"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding status %d", resp.StatusCode)
	}

	var out struct {
		Vectors []*[]float64 `json:"vectors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Vectors) == 0 || out.Vectors[0] == nil {
		return nil, fmt.Errorf("no vector")
	}
	return *out.Vectors[0], nil
}
