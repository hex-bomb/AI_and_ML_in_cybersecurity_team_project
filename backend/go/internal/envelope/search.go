package domain

// SearchCandidate is one branch (BM25 or kNN) result without the vector.
type SearchCandidate struct {
	ChunkID            string
	TenantID           string
	KBID               string
	DocumentID         string
	VersionID          string
	ChunkNumber        int
	Title              string
	Text               string
	Locator            Locator
	EmbeddingProfileID string
	Score              float64
	SourceURL          *string
	DownloadURL        string
}

// SearchFilter is the mandatory index filter. Empty AllowedDocumentIDs means
// "nothing is allowed" and must return [] without a network call.
type SearchFilter struct {
	TenantID           string
	KBID               string
	EmbeddingProfileID string
	AllowedDocumentIDs []string
}

func (f SearchFilter) AllowsNothing() bool {
	return len(f.AllowedDocumentIDs) == 0
}

// IndexingResult is the version indexing outcome.
type IndexingResult struct {
	DocumentID         string
	VersionID          string
	EmbeddingProfileID string
	ChunkCount         int
	Status             string
}

const (
	IndexingStatusReady = "ready"
)

func NewIndexingResult(documentID, versionID, profileID string, chunkCount int) IndexingResult {
	return IndexingResult{
		DocumentID:         documentID,
		VersionID:          versionID,
		EmbeddingProfileID: profileID,
		ChunkCount:         chunkCount,
		Status:             IndexingStatusReady,
	}
}

// Citation is an answer source with a server-verified fragment.
type Citation struct {
	Number      int
	ChunkID     string
	DocumentID  string
	VersionID   string
	Title       string
	Quote       string
	Locator     Locator
	SourceURL   string
	DownloadURL string
}
