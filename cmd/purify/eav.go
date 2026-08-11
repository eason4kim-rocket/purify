package main

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/use-agent/purify/config"
	extractdomain "github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/verify/eav"
)

var errManagedEAVUnavailable = errors.New("managed entity attribution is unavailable")

// managedSourceJudgeRuntime owns the process-scoped EAV transport separately
// from the request-scoped LLM client. Embedding the judge keeps the extract
// service boundary transport-neutral while Close releases only managed idles.
type managedSourceJudgeRuntime struct {
	extractdomain.SourceJudge
	closeHTTP func()
	closeOnce sync.Once
}

func (runtime *managedSourceJudgeRuntime) Close() {
	if runtime == nil {
		return
	}
	runtime.closeOnce.Do(func() {
		if runtime.closeHTTP != nil {
			runtime.closeHTTP()
		}
	})
}

// newManagedSourceJudgeRuntime constructs a hardened, independently owned HTTP
// and LLM client around the injected managed-only policy. Disabled EAV remains
// inert and does not require or construct network state.
func newManagedSourceJudgeRuntime(
	cfg config.EAVConfig,
	policy *publicnet.Policy,
) (*managedSourceJudgeRuntime, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := config.ValidateEAVConfig(cfg); err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, errManagedEAVUnavailable
	}
	httpClient, err := llm.NewPublicHTTPClient(policy, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: construct HTTP client: %w", errManagedEAVUnavailable, err)
	}
	judge, err := newManagedSourceJudge(cfg, llm.NewClient(httpClient))
	if err != nil {
		httpClient.CloseIdleConnections()
		return nil, err
	}
	return &managedSourceJudgeRuntime{
		SourceJudge: judge,
		closeHTTP:   httpClient.CloseIdleConnections,
	}, nil
}

// newManagedSourceJudge assembles the production entity-attribution judge:
// an LLM-backed blind extractor behind a content-keyed cache, an optional
// LLM referee, and the eav orchestration. Disabled configuration returns a
// nil judge without constructing provider state.
func newManagedSourceJudge(
	cfg config.EAVConfig,
	extractor managedStructuredExtractor,
) (extractdomain.SourceJudge, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := config.ValidateEAVConfig(cfg); err != nil {
		return nil, err
	}
	if extractor == nil {
		return nil, errManagedEAVUnavailable
	}
	params := llm.ExtractParams{
		APIKey:  strings.TrimSpace(cfg.APIKey),
		Model:   strings.TrimSpace(cfg.Model),
		BaseURL: strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
	}
	entityExtractor := newCachingEntityExtractor(
		&managedEntityExtractor{extractor: extractor, params: params},
		cfg.CacheEntries,
	)
	var referee eav.Referee
	if cfg.RefereeEnabled {
		referee = &managedEntityReferee{extractor: extractor, params: params}
	}
	judge, err := eav.NewJudge(eav.Config{Extractor: entityExtractor, Referee: referee})
	if err != nil {
		return nil, err
	}
	return judge, nil
}

// managedEntityExtractor is the LLM-backed blind extraction adapter. The llm
// client owns the transport system prompt, so the eav instruction rides at
// the head of the user content; the recording-equivalent reply is what
// matters, and DecodeExtractionReply validates it either way.
type managedEntityExtractor struct {
	extractor managedStructuredExtractor
	params    llm.ExtractParams
}

func (adapter *managedEntityExtractor) ExtractEntities(
	ctx context.Context,
	doc eav.Document,
	slate []eav.Candidate,
) (eav.DocumentEntities, error) {
	if adapter == nil || adapter.extractor == nil || ctx == nil {
		return eav.DocumentEntities{}, errManagedEAVUnavailable
	}
	if err := ctx.Err(); err != nil {
		return eav.DocumentEntities{}, err
	}
	content := eav.ExtractionSystemPrompt + "\n\n" + eav.BuildExtractionInput(doc, slate)
	result, err := adapter.extractor.Extract(ctx, content, json.RawMessage(eav.ExtractionReplySchema), adapter.params)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return eav.DocumentEntities{}, ctxErr
		}
		return eav.DocumentEntities{}, err
	}
	if result == nil {
		return eav.DocumentEntities{}, errManagedEAVUnavailable
	}
	return eav.DecodeExtractionReply(result.Data)
}

// managedEntityReferee is the LLM-backed gray-zone referee adapter. It is
// deliberately uncached: its answer depends on the subject, and it only runs
// for the gray zone.
type managedEntityReferee struct {
	extractor managedStructuredExtractor
	params    llm.ExtractParams
}

func (adapter *managedEntityReferee) SameReferent(
	ctx context.Context,
	subject eav.Subject,
	entity eav.Entity,
	doc eav.Document,
) (eav.RefereeVerdict, error) {
	if adapter == nil || adapter.extractor == nil || ctx == nil {
		return eav.RefereeVerdict{}, errManagedEAVUnavailable
	}
	if err := ctx.Err(); err != nil {
		return eav.RefereeVerdict{}, err
	}
	content := eav.RefereeSystemPrompt + "\n\n" + eav.BuildRefereeInput(subject, entity, doc)
	result, err := adapter.extractor.Extract(ctx, content, json.RawMessage(eav.RefereeReplySchema), adapter.params)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return eav.RefereeVerdict{}, ctxErr
		}
		return eav.RefereeVerdict{}, err
	}
	if result == nil {
		return eav.RefereeVerdict{}, errManagedEAVUnavailable
	}
	return eav.DecodeRefereeReply(result.Data)
}

// cachingEntityExtractor memoizes successful blind extractions by document
// content. The key hashes the exact blind input (URL, title, head window),
// which is content-addressed by construction: revisits of an unchanged
// snapshot never pay a second LLM call. Only successes are cached, and
// cached results are deep-cloned on the way out so gating can never share
// state across requests.
type cachingEntityExtractor struct {
	inner   eav.EntityExtractor
	mu      sync.Mutex
	limit   int
	entries map[string]*list.Element
	order   list.List
}

type entityCacheEntry struct {
	key   string
	value eav.DocumentEntities
}

func newCachingEntityExtractor(inner eav.EntityExtractor, limit int) *cachingEntityExtractor {
	return &cachingEntityExtractor{
		inner:   inner,
		limit:   limit,
		entries: make(map[string]*list.Element, limit),
	}
}

func (cache *cachingEntityExtractor) ExtractEntities(
	ctx context.Context,
	doc eav.Document,
	slate []eav.Candidate,
) (eav.DocumentEntities, error) {
	if cache == nil || cache.inner == nil {
		return eav.DocumentEntities{}, errManagedEAVUnavailable
	}
	key := entityCacheKey(doc)
	cache.mu.Lock()
	if element, ok := cache.entries[key]; ok {
		cache.order.MoveToFront(element)
		value := cloneDocumentEntities(element.Value.(*entityCacheEntry).value)
		cache.mu.Unlock()
		return value, nil
	}
	cache.mu.Unlock()

	extracted, err := cache.inner.ExtractEntities(ctx, doc, slate)
	if err != nil {
		return eav.DocumentEntities{}, err
	}

	cache.mu.Lock()
	if _, exists := cache.entries[key]; !exists && cache.limit > 0 {
		element := cache.order.PushFront(&entityCacheEntry{key: key, value: cloneDocumentEntities(extracted)})
		cache.entries[key] = element
		for len(cache.entries) > cache.limit {
			oldest := cache.order.Back()
			if oldest == nil {
				break
			}
			entry := oldest.Value.(*entityCacheEntry)
			delete(cache.entries, entry.key)
			cache.order.Remove(oldest)
		}
	}
	cache.mu.Unlock()
	return extracted, nil
}

func entityCacheKey(doc eav.Document) string {
	digest := sha256.New()
	digest.Write([]byte(doc.URL))
	digest.Write([]byte{0})
	digest.Write([]byte(doc.Title))
	digest.Write([]byte{0})
	digest.Write([]byte(doc.Cleaned))
	return string(digest.Sum(nil))
}

func cloneDocumentEntities(value eav.DocumentEntities) eav.DocumentEntities {
	cloned := eav.DocumentEntities{}
	if value.Primary != nil {
		primary := cloneEntity(*value.Primary)
		cloned.Primary = &primary
	}
	if len(value.Secondary) > 0 {
		cloned.Secondary = make([]eav.Entity, len(value.Secondary))
		for index := range value.Secondary {
			cloned.Secondary[index] = cloneEntity(value.Secondary[index])
		}
	}
	return cloned
}

func cloneEntity(value eav.Entity) eav.Entity {
	value.Name = strings.Clone(value.Name)
	value.Quote = strings.Clone(value.Quote)
	value.Kind = eav.Kind(strings.Clone(string(value.Kind)))
	if value.Aliases != nil {
		aliases := make([]string, len(value.Aliases))
		for index := range value.Aliases {
			aliases[index] = strings.Clone(value.Aliases[index])
		}
		value.Aliases = aliases
	}
	return value
}
