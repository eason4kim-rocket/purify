package search

import (
	"context"
	"reflect"
	"testing"
	"time"
)

type contractProvider struct {
	query ProviderQuery
}

func (*contractProvider) Name() string { return "contract" }

func (provider *contractProvider) Search(_ context.Context, query ProviderQuery) ([]ProviderResult, error) {
	provider.query = query
	return []ProviderResult{{Rank: 1, Title: "result", URL: "https://example.com/"}}, nil
}

var _ Provider = (*contractProvider)(nil)

func TestProviderContractCarriesOnlyBaselineInputs(t *testing.T) {
	provider := &contractProvider{}
	query := ProviderQuery{Text: "purify search", Limit: 10, Freshness: FreshnessWeek}
	results, err := provider.Search(context.Background(), query)
	if err != nil || !reflect.DeepEqual(provider.query, query) || len(results) != 1 {
		t.Fatalf("Search() = %#v, %v; query=%#v", results, err, provider.query)
	}
	if provider.Name() != "contract" {
		t.Fatalf("Name() = %q", provider.Name())
	}
}

func TestProviderResultPointerSemantics(t *testing.T) {
	score := 0.0
	publishedAt := time.Date(2026, time.August, 10, 9, 0, 0, 0, time.FixedZone("test", 8*60*60))
	result := ProviderResult{
		Rank: 1, Score: &score, Title: "title", URL: "https://example.com/", Snippet: "snippet", PublishedAt: &publishedAt,
	}
	if result.Score == nil || *result.Score != 0 || result.PublishedAt == nil || !result.PublishedAt.Equal(publishedAt) {
		t.Fatalf("ProviderResult pointer fields = %#v", result)
	}

	withoutOptionals := ProviderResult{Rank: 2, URL: "https://example.net/"}
	if withoutOptionals.Score != nil || withoutOptionals.PublishedAt != nil {
		t.Fatalf("absent provider optionals = %#v", withoutOptionals)
	}
}

func TestProviderResourceContract(t *testing.T) {
	if MaxProviderResults != 20 || MaxProviderNameBytes != 64 ||
		MaxProviderTitleBytes != 16<<10 || MaxProviderSnippetBytes != 64<<10 ||
		MaxProviderMetadataBytes != 2<<20 {
		t.Fatalf("provider limits = results:%d name:%d title:%d snippet:%d metadata:%d",
			MaxProviderResults, MaxProviderNameBytes, MaxProviderTitleBytes,
			MaxProviderSnippetBytes, MaxProviderMetadataBytes)
	}
}

func TestProviderContractFieldClassificationStaysCurrent(t *testing.T) {
	assertProviderFields(t, reflect.TypeOf(ProviderQuery{}), []string{"Text", "Limit", "Freshness"})
	assertProviderFields(t, reflect.TypeOf(ProviderResult{}), []string{
		"Rank", "Score", "Title", "URL", "Snippet", "PublishedAt",
	})
	assertProviderFields(t, reflect.TypeOf(ProviderError{}), []string{"Kind", "StatusCode", "Err"})
}

func assertProviderFields(t *testing.T, structure reflect.Type, expected []string) {
	t.Helper()
	actual := make([]string, structure.NumField())
	for index := 0; index < structure.NumField(); index++ {
		actual[index] = structure.Field(index).Name
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("%s fields changed: got %v, want %v", structure.Name(), actual, expected)
	}
}
