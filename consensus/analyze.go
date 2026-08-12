package consensus

import (
	"context"
	"fmt"
	"sort"
)

const independenceAnchorCancelChunk = 32

// IndependenceMember is one canonical URL's projection onto the production
// independence forest. Representative is the lexical component root. Parent
// fields are empty for that root.
type IndependenceMember struct {
	URL            string
	Representative string
	ComponentSize  int
	ParentURL      string
	ParentReason   FoldReason
}

// IndependenceAnalysis is the verdict-neutral, read-only view of the same
// component plan Merge uses. It does not materialize winners or wire JSON.
type IndependenceAnalysis struct {
	Members          []IndependenceMember
	EffectiveSources int
}

// AnalyzeIndependence prepares sources and projects their independence forest.
// Cancel is observed at each source, after each bounded anchor-admission
// chunk, and at every pairwise comparison. Merge keeps the no-cancel path.
func AnalyzeIndependence(ctx context.Context, results []SourceResult) (IndependenceAnalysis, error) {
	if ctx == nil {
		return IndependenceAnalysis{}, fmt.Errorf("%w: context is required", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return IndependenceAnalysis{}, err
	}
	prepared, err := prepareSourcesForIndependence(ctx, results)
	if err != nil {
		return IndependenceAnalysis{}, err
	}
	plan, err := buildIndependencePlanContext(ctx, prepared)
	if err != nil {
		return IndependenceAnalysis{}, err
	}
	return projectIndependence(prepared, plan), nil
}

func prepareSourcesForIndependence(ctx context.Context, results []SourceResult) ([]preparedSource, error) {
	if len(results) == 0 {
		return nil, fmt.Errorf("%w: at least one source is required", ErrInvalidInput)
	}
	if len(results) > MaxSources {
		return nil, fmt.Errorf("%w: sources exceed %d", ErrResourceLimit, MaxSources)
	}
	prepared := make([]preparedSource, 0, len(results))
	byURL := make(map[string]int, len(results))
	totalDataBytes := 0
	totalMetadataBytes := 0
	for index, result := range results {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source, dataBytes, metadataBytes, err := prepareSourceContext(ctx, index, result)
		if err != nil {
			return nil, err
		}
		if dataBytes > MaxTotalDataBytes-totalDataBytes {
			return nil, fmt.Errorf("%w: source data exceeds %d aggregate bytes", ErrResourceLimit, MaxTotalDataBytes)
		}
		totalDataBytes += dataBytes
		if metadataBytes > MaxTotalMetadataBytes-totalMetadataBytes {
			return nil, fmt.Errorf("%w: source metadata exceeds %d aggregate bytes", ErrResourceLimit, MaxTotalMetadataBytes)
		}
		totalMetadataBytes += metadataBytes
		if priorIndex, duplicate := byURL[source.url]; duplicate {
			if !sourcesEqual(prepared[priorIndex], source) {
				return nil, fmt.Errorf("%w: canonical URL %q has non-identical results", ErrDuplicateSourceConflict, source.url)
			}
			continue
		}
		byURL[source.url] = len(prepared)
		prepared = append(prepared, source)
	}
	sort.Slice(prepared, func(i, j int) bool { return prepared[i].url < prepared[j].url })
	return prepared, nil
}

func projectIndependence(sources []preparedSource, plan independencePlan) IndependenceAnalysis {
	sizes := make(map[int]int, len(sources))
	for _, representative := range plan.components {
		sizes[representative]++
	}
	members := make([]IndependenceMember, len(sources))
	for index, source := range sources {
		representative := plan.components[index]
		member := IndependenceMember{
			URL:            source.url,
			Representative: sources[representative].url,
			ComponentSize:  sizes[representative],
		}
		if parent := plan.parent[index]; parent >= 0 {
			member.ParentURL = sources[parent].url
			member.ParentReason = plan.parentReason[index]
		}
		members[index] = member
	}
	return IndependenceAnalysis{Members: members, EffectiveSources: len(sizes)}
}
