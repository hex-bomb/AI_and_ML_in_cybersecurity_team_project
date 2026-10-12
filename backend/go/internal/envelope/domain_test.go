package domain

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

func nowFixture() time.Time {
	return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
}

func TestIndexingConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *IndexingConfig)
		wantErr bool
	}{
		{"корректный e5", func(*IndexingConfig) {}, false},
		{"нулевая размерность", func(c *IndexingConfig) { c.EmbeddingDimension = 0 }, true},
		{"отрицательная размерность", func(c *IndexingConfig) { c.EmbeddingDimension = -1 }, true},
		{"нулевой max_tokens", func(c *IndexingConfig) { c.ChunkMaxTokens = 0 }, true},
		{"отрицательный overlap", func(c *IndexingConfig) { c.ChunkOverlapTokens = -1 }, true},
		{"overlap равен max_tokens", func(c *IndexingConfig) {
			c.ChunkOverlapTokens = c.ChunkMaxTokens
		}, true},
		{"overlap больше max_tokens", func(c *IndexingConfig) {
			c.ChunkOverlapTokens = c.ChunkMaxTokens + 1
		}, true},
		{"overlap на единицу меньше max_tokens", func(c *IndexingConfig) {
			c.ChunkOverlapTokens = c.ChunkMaxTokens - 1
		}, false},
		{"нулевой overlap", func(c *IndexingConfig) { c.ChunkOverlapTokens = 0 }, false},
		{"dot вместо cosine", func(c *IndexingConfig) { c.Distance = "dot" }, true},
		{"пустая модель", func(c *IndexingConfig) { c.EmbeddingModel = "  " }, true},
		{"пустой токенизатор", func(c *IndexingConfig) { c.TokenizerID = "" }, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := e5Config()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("ожидалась ошибка, получено nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ожидался nil, получено %v", err)
			}
			if tc.wantErr {
				if !errors.Is(err, ErrConfigurationInvalid) {
					t.Errorf("код ошибки %v, ожидался %s", err, CodeConfigurationInvalid)
				}
				var appErr *AppError
				if !errors.As(err, &appErr) || appErr.Status != 409 {
					t.Errorf("HTTP-статус не 409: %v", err)
				}
			}
		})
	}
}

func TestSearchFilterAllowsNothing(t *testing.T) {
	empty := SearchFilter{TenantID: "t", KBID: "k", EmbeddingProfileID: "p"}
	if !empty.AllowsNothing() {
		t.Error("пустой AllowedDocumentIDs должен означать AllowsNothing")
	}
	filled := empty
	filled.AllowedDocumentIDs = []string{"doc-1"}
	if filled.AllowsNothing() {
		t.Error("непустой AllowedDocumentIDs не должен означивать AllowsNothing")
	}
}

func TestACLGrants(t *testing.T) {
	p := Principal{
		TenantID: uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		UserID:   "alice",
		Roles:    []string{"reader"},
		Groups:   []string{"team-a"},
	}

	cases := []struct {
		name string
		acl  ACL
		want bool
	}{
		{"пустой ACL открыт всем", ACL{}, true},
		{"по роли", ACL{Roles: []string{"reader"}}, true},
		{"по другой роли", ACL{Roles: []string{"admin"}}, false},
		{"по пользователю", ACL{Users: []string{"alice"}}, true},
		{"по чужому пользователю", ACL{Users: []string{"bob"}}, false},
		{"по группе", ACL{Groups: []string{"team-a"}}, true},
		{"по чужой группе", ACL{Groups: []string{"team-b"}}, false},
		{"любое из условий", ACL{Users: []string{"bob"}, Roles: []string{"reader"}}, true},
		{"ни одно условие не выполнено", ACL{Users: []string{"bob"}, Groups: []string{"team-b"}}, false},
		{"регистрозависимо", ACL{Roles: []string{"READER"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.acl.Grants(p); got != tc.want {
				t.Errorf("Grants() = %v, ожидалось %v", got, tc.want)
			}
		})
	}
}

func TestLocalPrincipalMatchesPython(t *testing.T) {
	p := LocalPrincipal()
	if p.TenantID.String() != "11111111-1111-4111-8111-111111111111" {
		t.Errorf("tenant %s, ожидалось 11111111-1111-4111-8111-111111111111", p.TenantID)
	}
	if p.UserID != "local-dev" {
		t.Errorf("user %q, ожидалось local-dev", p.UserID)
	}
	if !p.HasRole("admin") {
		t.Error("локальный principal должен иметь роль admin")
	}
	if p.HasRole("") {
		t.Error("пустой роли у principal быть не должно")
	}
}

func TestLocalPrincipalReturnsFreshSlices(t *testing.T) {
	a := LocalPrincipal()
	b := LocalPrincipal()

	a.Roles[0] = "reader"
	a.Groups = append(a.Groups, "мусор")

	if b.Roles[0] != "admin" {
		t.Errorf("роли разделяются между вызовами: %q", b.Roles[0])
	}
	if len(b.Groups) != 0 {
		t.Errorf("группы разделяются между вызовами: %v", b.Groups)
	}
}

func TestParsedDocumentBlockLookup(t *testing.T) {
	first := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	second := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	doc := ParsedDocument{
		Blocks: []Block{
			{BlockID: first, Position: 0, Kind: "paragraph", Text: "раз"},
			{BlockID: second, Position: 1, Kind: "table", Text: "два"},
		},
	}

	if got, ok := doc.BlockByID(second); !ok || got.Text != "два" {
		t.Errorf("BlockByID(second) = %+v, %v", got, ok)
	}
	if _, ok := doc.BlockByID(uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")); ok {
		t.Error("BlockByID несуществующего блока вернул ok=true")
	}
}

func TestSourceURLDistinguishesEmptyAndMissing(t *testing.T) {
	missing := ParsedDocument{}
	if _, ok := missing.SourceURLValue(); ok {
		t.Error("отсутствующий source_url вернул ok=true")
	}
	empty := ""
	withEmpty := ParsedDocument{SourceURL: &empty}
	got, ok := withEmpty.SourceURLValue()
	if !ok {
		t.Error("указаный пустой source_url потерялся")
	}
	if got != "" {
		t.Errorf("source_url = %q, ожидалась пустая строка", got)
	}
}

func TestJobLeaseExpiryBoundary(t *testing.T) {
	base := nowFixture()
	lease := JobLease{LeaseExpiresAt: base}

	if lease.ExpiredAt(base) != true {
		t.Error("lease, истёкший ровно сейчас, не считается просроченным")
	}
	if lease.ExpiredAt(base.Add(-time.Millisecond)) != false {
		t.Error("lease с запасом в миллисекунду не должен считаться просроченным")
	}
	if lease.ExpiredAt(base.Add(time.Millisecond)) != true {
		t.Error("lease, истёкший раньше, не считается просроченным")
	}
}

func TestHeartbeatIntervalFitsLease(t *testing.T) {
	if HeartbeatInterval*3 > LeaseDuration {
		t.Errorf("LeaseDuration %s не переживает три пропущенных heartbeat по %s",
			LeaseDuration, HeartbeatInterval)
	}
	if HeartbeatInterval*2 >= LeaseDuration {
		t.Errorf("LeaseDuration %s слишком близок к двум heartbeat по %s",
			LeaseDuration, HeartbeatInterval)
	}
}

func TestJobLeaseStageHelpers(t *testing.T) {
	index := JobLease{Stage: StageIndex}
	if !index.IsIndexStage() {
		t.Error("стадия index не распознана")
	}
	convert := JobLease{Stage: StageConvert}
	if convert.IsIndexStage() {
		t.Error("стадия convert ошибочно распознана как index")
	}
	if !convert.IsConvertStage() {
		t.Error("стадия convert не распознана как convert")
	}

	// Все три стадии обслуживает Go: convert ставит приём файла, index — стадия
	// convert, delete — MarkDeleted. Без convert документ остался бы без
	// parsed.json навсегда, а без delete чанки удалённого документа остались бы
	// в индексе навсегда (§9).
	stages := GoStages()
	for _, want := range []string{StageConvert, StageIndex, StageDelete} {
		if !slices.Contains(stages, want) {
			t.Errorf("в GoStages() нет стадии %q", want)
		}
	}
	for _, stage := range stages {
		switch stage {
		case StageConvert, StageIndex, StageDelete:
		default:
			t.Errorf("в GoStages() чужая стадия %q: её обслуживает другой воркер", stage)
		}
	}
	if deleteLease := (JobLease{Stage: StageDelete}); !deleteLease.IsDeleteStage() {
		t.Error("стадия delete не распознана как delete")
	}

	if _, ok := index.VersionIDValue(); ok {
		t.Error("VersionIDValue() вернул ok при nil VersionID")
	}
	versionID := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	withVersion := JobLease{VersionID: &versionID}
	got, ok := withVersion.VersionIDValue()
	if !ok || got != versionID {
		t.Errorf("VersionIDValue() = %v, %v", got, ok)
	}
}
