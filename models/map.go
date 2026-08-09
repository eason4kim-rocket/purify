package models

// MapRequest is the payload for POST /api/v1/map.
type MapRequest struct {
	// URL is the target site to discover URLs for. Required.
	URL string `json:"url" binding:"required,url"`
}

// MapResponse is the response for POST /api/v1/map.
type MapResponse struct {
	Success  bool            `json:"success"`
	URLs     []string        `json:"urls"`
	Total    int             `json:"total"`
	Warnings []MapWarning    `json:"warnings,omitempty"`
	Sources  *MapSourceStats `json:"sources,omitempty"`
	Error    *ErrorDetail    `json:"error,omitempty"`
}

// MapWarning reports one failed or truncated discovery source while allowing
// successful sources to contribute URLs.
type MapWarning struct {
	Source  string `json:"source"`
	URL     string `json:"url,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// MapSourceStats summarizes the bounded work performed by each discovery
// source.
type MapSourceStats struct {
	SitemapFiles   int  `json:"sitemap_files"`
	SitemapURLs    int  `json:"sitemap_urls"`
	RobotsSitemaps int  `json:"robots_sitemaps"`
	HomeLinks      int  `json:"home_links"`
	Truncated      bool `json:"truncated"`
}
