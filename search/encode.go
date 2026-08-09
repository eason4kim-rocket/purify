package search

import (
	"context"
	"encoding/json"

	"github.com/use-agent/purify/models"
)

// EncodeSearchResponse performs final JSON materialization under a bounded
// service-wide slot and rejects a response one byte beyond the public 32 MiB
// contract. Bytes produced after cancellation are never returned.
func (service *Service) EncodeSearchResponse(ctx context.Context, response *models.SearchResponse) (encoded []byte, err error) {
	if ctx == nil {
		return nil, invalidSearchInput("search response context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, searchTimeout(err)
	}
	if service == nil || service.encodingSlots == nil || response == nil {
		return nil, models.NewScrapeError(models.ErrCodeInternal, "search response encoding failed", nil)
	}
	if err := acquireSearchSlot(ctx, service.encodingSlots); err != nil {
		return nil, searchTimeout(err)
	}
	defer releaseSearchSlot(service.encodingSlots)
	defer func() {
		if recover() != nil {
			encoded = nil
			err = models.NewScrapeError(models.ErrCodeInternal, "search response encoding failed", nil)
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, searchTimeout(err)
	}

	encoder := service.encodeSearch
	if encoder == nil {
		encoder = json.Marshal
	}
	encoded, encodeErr := encoder(response)
	if err := ctx.Err(); err != nil {
		return nil, searchTimeout(err)
	}
	if encodeErr != nil || len(encoded) == 0 {
		return nil, models.NewScrapeError(models.ErrCodeInternal, "search response encoding failed", nil)
	}
	if len(encoded) > models.MaxSearchResponseBytes {
		return nil, models.NewScrapeError(models.ErrCodeInternal, "search response exceeds its output budget", nil)
	}
	valid := json.Valid(encoded)
	if err := ctx.Err(); err != nil {
		return nil, searchTimeout(err)
	}
	if !valid {
		return nil, models.NewScrapeError(models.ErrCodeInternal, "search response encoding failed", nil)
	}
	return encoded, nil
}
