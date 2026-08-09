package scraper

import (
	"net/http"
	"time"

	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/models"
)

// FetchRequestFromScrapeRequest maps every fetch-layer option from the public
// scrape model. Cleaner-only options intentionally remain outside engine.
func FetchRequestFromScrapeRequest(req *models.ScrapeRequest, timeout time.Duration) *engine.FetchRequest {
	if req == nil {
		return nil
	}
	cookies := make([]http.Cookie, len(req.Cookies))
	for i, cookie := range req.Cookies {
		cookies[i] = http.Cookie{
			Name:   cookie.Name,
			Value:  cookie.Value,
			Domain: cookie.Domain,
			Path:   cookie.Path,
		}
	}
	actions := make([]engine.Action, len(req.Actions))
	for i, action := range req.Actions {
		actions[i] = engine.Action{
			Type:         action.Type,
			Selector:     action.Selector,
			Milliseconds: action.Milliseconds,
			Direction:    action.Direction,
			Amount:       action.Amount,
			Code:         action.Code,
		}
	}
	return &engine.FetchRequest{
		URL:                req.URL,
		Headers:            cloneHeaders(req.Headers),
		Cookies:            cookies,
		Timeout:            timeout,
		ProxyURL:           req.ProxyURL,
		Stealth:            req.Stealth,
		WaitForNetworkIdle: cloneBool(req.WaitForNetworkIdle),
		RemoveOverlays:     req.RemoveOverlays,
		BlockAds:           req.BlockAds,
		Actions:            actions,
		CDPURL:             req.CDPURL,
		MaximumBodyBytes:   req.MaximumBodyBytes,
	}
}

// ScrapeRequestFromFetchRequest is the Rod callback adapter. The parent
// context retains the exact duration budget; the integer timeout is rounded up
// only because the public model stores seconds.
func ScrapeRequestFromFetchRequest(req *engine.FetchRequest) *models.ScrapeRequest {
	if req == nil {
		return nil
	}
	cookies := make([]models.Cookie, len(req.Cookies))
	for i, cookie := range req.Cookies {
		cookies[i] = models.Cookie{
			Name:   cookie.Name,
			Value:  cookie.Value,
			Domain: cookie.Domain,
			Path:   cookie.Path,
		}
	}
	actions := make([]models.Action, len(req.Actions))
	for i, action := range req.Actions {
		actions[i] = models.Action{
			Type:         action.Type,
			Selector:     action.Selector,
			Milliseconds: action.Milliseconds,
			Direction:    action.Direction,
			Amount:       action.Amount,
			Code:         action.Code,
		}
	}
	return &models.ScrapeRequest{
		URL:                req.URL,
		WaitForNetworkIdle: cloneBool(req.WaitForNetworkIdle),
		Timeout:            durationSecondsCeil(req.Timeout),
		Stealth:            req.Stealth,
		ProxyURL:           req.ProxyURL,
		Headers:            cloneHeaders(req.Headers),
		Cookies:            cookies,
		Actions:            actions,
		RemoveOverlays:     req.RemoveOverlays,
		BlockAds:           req.BlockAds,
		CDPURL:             req.CDPURL,
		MaximumBodyBytes:   req.MaximumBodyBytes,
	}
}

func cloneHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	clone := make(map[string]string, len(headers))
	for key, value := range headers {
		clone[key] = value
	}
	return clone
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func durationSecondsCeil(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int((duration + time.Second - 1) / time.Second)
}
