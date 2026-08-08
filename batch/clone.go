package batch

import (
	"slices"

	"github.com/use-agent/purify/models"
)

func cloneScrapeResponse(source *models.ScrapeResponse) *models.ScrapeResponse {
	if source == nil {
		return nil
	}

	cloned := *source
	cloned.Links.Internal = slices.Clone(source.Links.Internal)
	cloned.Links.External = slices.Clone(source.Links.External)
	cloned.Images = slices.Clone(source.Images)
	if source.Error != nil {
		errorDetail := *source.Error
		cloned.Error = &errorDetail
	}
	if source.Quality != nil {
		qualityInfo := *source.Quality
		qualityInfo.Warnings = slices.Clone(source.Quality.Warnings)
		qualityInfo.FetchAttempts = slices.Clone(source.Quality.FetchAttempts)
		cloned.Quality = &qualityInfo
	}
	return &cloned
}

func cloneResults(source []*models.ScrapeResponse) []*models.ScrapeResponse {
	if source == nil {
		return nil
	}
	cloned := make([]*models.ScrapeResponse, len(source))
	for index, response := range source {
		cloned[index] = cloneScrapeResponse(response)
	}
	return cloned
}

func cloneStatus(source models.BatchStatusResponse) models.BatchStatusResponse {
	source.Results = cloneResults(source.Results)
	return source
}
