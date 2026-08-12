package rerankeval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const (
	PriceEvidenceSchemaVersion = "rerank-price-evidence-v1"
	PriceKindProviderCall      = "provider_call"

	pricePublisherBrave        = "brave"
	priceProductBraveWebSearch = "web_search"
	pricePlanBraveSearch       = "search"
	priceUnitRequest           = "request"
	priceSourceBraveHTML       = "brave_search_pricing_html_v1"
	priceManifestName          = "manifest.json"
	maxPriceSourceBytes        = 1 << 20
	priceEvidenceIDDomain      = "rerank-price-evidence-v1\x00"
)

var ErrInvalidPriceEvidence = errors.New("rerankeval: invalid price evidence")

// PriceMoney is an exact decimal amount with nine fractional digits. Price
// evidence never accepts a floating-point operand from a fixture.
type PriceMoney struct {
	Units int64 `json:"units"`
	Nanos int32 `json:"nanos"`
}

type PriceEvidenceSource struct {
	Role              string `json:"role"`
	Path              string `json:"path"`
	CanonicalHTTPSURL string `json:"canonical_https_url"`
	MediaType         string `json:"media_type"`
	BodySHA256        string `json:"body_sha256"`
	ResponseDate      string `json:"response_date"`
	ETag              string `json:"etag,omitempty"`
	LastModified      string `json:"last_modified,omitempty"`
}

// PriceEvidenceManifest describes one and only one billing operand. A
// provider-call artifact cannot also claim a GPU-hour or managed rerank price.
type PriceEvidenceManifest struct {
	SchemaVersion string                `json:"schema_version"`
	ArtifactID    string                `json:"artifact_id"`
	Kind          string                `json:"kind"`
	CapturedAt    string                `json:"captured_at"`
	PriceDate     string                `json:"price_date"`
	Currency      string                `json:"currency"`
	Publisher     string                `json:"publisher"`
	Product       string                `json:"product"`
	PlanOrSKU     string                `json:"plan_or_sku"`
	Offering      string                `json:"offering"`
	Region        string                `json:"region"`
	BillingUnit   string                `json:"billing_unit"`
	UnitQuantity  int64                 `json:"unit_quantity"`
	Amount        PriceMoney            `json:"amount"`
	Basis         string                `json:"basis"`
	SourceFormat  string                `json:"source_format"`
	Sources       []PriceEvidenceSource `json:"sources"`
}

// PriceEvidence is the validated manifest plus the exact nanodollar cost of
// one billing unit derived from publisher bytes. Artifact identity proves
// integrity and provenance only; it is not a publisher signature.
type PriceEvidence struct {
	Manifest      PriceEvidenceManifest
	UnitCostNanos int64
}

// DecodePriceManifest applies the same recursive strict-JSON rules as the
// replay corpus and accepts only the first, deliberately narrow Brave price
// profile. Future price kinds require their own parser and contract version.
func DecodePriceManifest(raw []byte) (PriceEvidenceManifest, error) {
	if len(raw) == 0 || len(raw) > MaxJSONLLineBytes || !utf8.Valid(raw) || bytes.Contains(raw, []byte{0xef, 0xbb, 0xbf}) ||
		!bytes.Equal(bytes.TrimSpace(raw), raw) {
		return PriceEvidenceManifest{}, ErrInvalidPriceEvidence
	}
	if err := validateJSON(raw); err != nil {
		return PriceEvidenceManifest{}, fmt.Errorf("%w: manifest JSON", ErrInvalidPriceEvidence)
	}
	fields, err := exactObject(raw, []string{
		"schema_version", "artifact_id", "kind", "captured_at", "price_date", "currency", "publisher", "product",
		"plan_or_sku", "offering", "region", "billing_unit", "unit_quantity", "amount", "basis", "source_format", "sources",
	}, nil)
	if err != nil {
		return PriceEvidenceManifest{}, fmt.Errorf("%w: manifest fields", ErrInvalidPriceEvidence)
	}
	var manifest PriceEvidenceManifest
	if err := json.Unmarshal(raw, &manifest); err != nil || !validPriceManifestHeader(manifest) {
		return PriceEvidenceManifest{}, ErrInvalidPriceEvidence
	}
	amountFields, err := exactObject(fields["amount"], []string{"units", "nanos"}, nil)
	if err != nil {
		return PriceEvidenceManifest{}, ErrInvalidPriceEvidence
	}
	units, unitsErr := jsonInt64(amountFields["units"])
	nanos, nanosErr := jsonInt(amountFields["nanos"])
	if unitsErr != nil || nanosErr != nil || units != manifest.Amount.Units || int32(nanos) != manifest.Amount.Nanos || nanos < 0 || nanos >= 1_000_000_000 {
		return PriceEvidenceManifest{}, ErrInvalidPriceEvidence
	}
	rawSources, err := rawArray(fields["sources"], 1, 8)
	if err != nil || len(rawSources) != len(manifest.Sources) {
		return PriceEvidenceManifest{}, ErrInvalidPriceEvidence
	}
	seenPaths := make(map[string]struct{}, len(rawSources))
	for index, rawSource := range rawSources {
		sourceFields, objectErr := exactObject(rawSource,
			[]string{"role", "path", "canonical_https_url", "media_type", "body_sha256", "response_date"},
			[]string{"etag", "last_modified"})
		if objectErr != nil || !validPriceSource(manifest.Sources[index], manifest.PriceDate, manifest.CapturedAt) {
			return PriceEvidenceManifest{}, ErrInvalidPriceEvidence
		}
		if _, duplicate := seenPaths[manifest.Sources[index].Path]; duplicate {
			return PriceEvidenceManifest{}, ErrInvalidPriceEvidence
		}
		seenPaths[manifest.Sources[index].Path] = struct{}{}
		if _, present := sourceFields["etag"]; present != (manifest.Sources[index].ETag != "") {
			return PriceEvidenceManifest{}, ErrInvalidPriceEvidence
		}
		if _, present := sourceFields["last_modified"]; present != (manifest.Sources[index].LastModified != "") {
			return PriceEvidenceManifest{}, ErrInvalidPriceEvidence
		}
	}
	identity, err := PriceEvidenceID(manifest)
	if err != nil || identity != manifest.ArtifactID {
		return PriceEvidenceManifest{}, ErrInvalidPriceEvidence
	}
	return clonePriceManifest(manifest), nil
}

// PriceEvidenceID returns the domain-separated canonical identity of a
// manifest with ArtifactID cleared. It does not authenticate the publisher.
func PriceEvidenceID(manifest PriceEvidenceManifest) (string, error) {
	manifest = clonePriceManifest(manifest)
	manifest.ArtifactID = ""
	if !validPriceManifestHeaderWithoutIdentity(manifest) || !validPriceSources(manifest.Sources, manifest.PriceDate, manifest.CapturedAt) {
		return "", ErrInvalidPriceEvidence
	}
	encoded, err := json.Marshal(manifest)
	if err != nil || len(encoded) > MaxJSONLLineBytes {
		return "", ErrInvalidPriceEvidence
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(priceEvidenceIDDomain))
	_, _ = digest.Write(encoded)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// LoadPriceEvidence verifies the exact directory inventory, source bytes,
// publisher-specific price extraction, and canonical manifest identity.
func LoadPriceEvidence(directory string) (PriceEvidence, error) {
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return PriceEvidence{}, ErrInvalidPriceEvidence
	}
	rootInfo, err := os.Lstat(directory)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm()&0o022 != 0 {
		return PriceEvidence{}, ErrInvalidPriceEvidence
	}
	initialInventory, err := priceDirectoryInventory(directory)
	if err != nil {
		return PriceEvidence{}, err
	}
	manifestRaw, err := readExactPriceFile(directory, priceManifestName, MaxJSONLLineBytes)
	if err != nil {
		return PriceEvidence{}, err
	}
	manifest, err := DecodePriceManifest(manifestRaw)
	if err != nil {
		return PriceEvidence{}, err
	}
	identity, err := PriceEvidenceID(manifest)
	if err != nil || identity != manifest.ArtifactID {
		return PriceEvidence{}, ErrInvalidPriceEvidence
	}
	if len(initialInventory) != len(manifest.Sources)+1 {
		return PriceEvidence{}, ErrInvalidPriceEvidence
	}
	expected := map[string]struct{}{priceManifestName: {}}
	var derived PriceMoney
	for _, source := range manifest.Sources {
		expected[source.Path] = struct{}{}
		body, readErr := readExactPriceFile(directory, source.Path, maxPriceSourceBytes)
		if readErr != nil {
			return PriceEvidence{}, readErr
		}
		if !utf8.Valid(body) || bytes.Contains(body, []byte{0xef, 0xbb, 0xbf}) || bytes.IndexByte(body, 0) >= 0 {
			return PriceEvidence{}, ErrInvalidPriceEvidence
		}
		digest := sha256.Sum256(body)
		if hex.EncodeToString(digest[:]) != source.BodySHA256 {
			return PriceEvidence{}, ErrInvalidPriceEvidence
		}
		parsed, parseErr := extractBraveSearchPrice(body)
		if parseErr != nil {
			return PriceEvidence{}, parseErr
		}
		derived = parsed
	}
	for _, name := range initialInventory {
		if _, present := expected[name]; !present {
			return PriceEvidence{}, ErrInvalidPriceEvidence
		}
	}
	finalInventory, err := priceDirectoryInventory(directory)
	rootAfter, rootErr := os.Lstat(directory)
	if err != nil || rootErr != nil || !os.SameFile(rootInfo, rootAfter) || rootAfter.Mode() != rootInfo.Mode() ||
		!rootAfter.ModTime().Equal(rootInfo.ModTime()) || !equalStrings(initialInventory, finalInventory) {
		return PriceEvidence{}, ErrInvalidPriceEvidence
	}
	if derived != manifest.Amount || manifest.UnitQuantity != 1_000 {
		return PriceEvidence{}, ErrInvalidPriceEvidence
	}
	totalNanos := manifest.Amount.Units*1_000_000_000 + int64(manifest.Amount.Nanos)
	if totalNanos <= 0 || totalNanos%manifest.UnitQuantity != 0 {
		return PriceEvidence{}, ErrInvalidPriceEvidence
	}
	return PriceEvidence{Manifest: clonePriceManifest(manifest), UnitCostNanos: totalNanos / manifest.UnitQuantity}, nil
}

func validPriceManifestHeader(manifest PriceEvidenceManifest) bool {
	return validDigest(manifest.ArtifactID) && validPriceManifestHeaderWithoutIdentity(manifest)
}

func validPriceManifestHeaderWithoutIdentity(manifest PriceEvidenceManifest) bool {
	captured, err := time.Parse(time.RFC3339, manifest.CapturedAt)
	if err != nil || captured.Format(time.RFC3339) != manifest.CapturedAt || captured.Location() != time.UTC ||
		captured.Format("2006-01-02") != manifest.PriceDate {
		return false
	}
	return manifest.SchemaVersion == PriceEvidenceSchemaVersion && manifest.Kind == PriceKindProviderCall &&
		manifest.Currency == "USD" && manifest.Publisher == pricePublisherBrave && manifest.Product == priceProductBraveWebSearch &&
		manifest.PlanOrSKU == pricePlanBraveSearch && manifest.Offering == "public_list" && manifest.Region == "global" &&
		manifest.BillingUnit == priceUnitRequest && manifest.UnitQuantity == 1_000 && manifest.Amount == (PriceMoney{Units: 5}) &&
		manifest.Basis == "public_list_excluding_credits_discounts_tax" && manifest.SourceFormat == priceSourceBraveHTML &&
		len(manifest.Sources) == 1
}

func validPriceSources(sources []PriceEvidenceSource, priceDate, capturedAt string) bool {
	if len(sources) != 1 {
		return false
	}
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		if !validPriceSource(source, priceDate, capturedAt) {
			return false
		}
		if _, duplicate := seen[source.Path]; duplicate {
			return false
		}
		seen[source.Path] = struct{}{}
	}
	return true
}

func validPriceSource(source PriceEvidenceSource, priceDate, capturedAt string) bool {
	if source.Role != "pricing_page" || source.Path != "brave-search-pricing.html" ||
		source.CanonicalHTTPSURL != "https://api-dashboard.search.brave.com/documentation/pricing" ||
		source.MediaType != "text/html; charset=utf-8" || !validDigest(source.BodySHA256) || !validPriceETag(source.ETag) {
		return false
	}
	response, err := httpDate(source.ResponseDate)
	captured, capturedErr := time.Parse(time.RFC3339, capturedAt)
	if err != nil || capturedErr != nil || response.Format("2006-01-02") != priceDate || response.After(captured) || captured.Sub(response) > 5*time.Minute {
		return false
	}
	if source.LastModified != "" {
		modified, modifiedErr := httpDate(source.LastModified)
		if modifiedErr != nil || modified.After(response) {
			return false
		}
	}
	return true
}

func validPriceETag(value string) bool {
	if value == "" {
		return true
	}
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return false
	}
	for _, character := range []byte(value[1 : len(value)-1]) {
		if character != 0x21 && (character < 0x23 || character > 0x7e) {
			return false
		}
	}
	return true
}

func httpDate(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC1123, value)
	if err != nil || parsed.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT") != value {
		return time.Time{}, ErrInvalidPriceEvidence
	}
	return parsed.UTC(), nil
}

func readExactPriceFile(directory, name string, maximum int64) ([]byte, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) {
		return nil, ErrInvalidPriceEvidence
	}
	path := filepath.Join(directory, name)
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o022 != 0 ||
		!priceFileHasSingleLink(before) {
		return nil, ErrInvalidPriceEvidence
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrInvalidPriceEvidence
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return nil, ErrInvalidPriceEvidence
	}
	defer func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() || !priceFileHasSingleLink(opened) ||
		opened.Size() < 1 || opened.Size() > maximum {
		return nil, ErrInvalidPriceEvidence
	}
	body, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(body)) != opened.Size() || int64(len(body)) > maximum {
		return nil, ErrInvalidPriceEvidence
	}
	after, err := file.Stat()
	pathAfter, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !os.SameFile(opened, after) || !os.SameFile(opened, pathAfter) ||
		after.Size() != opened.Size() || after.Mode() != opened.Mode() || !after.ModTime().Equal(opened.ModTime()) || !priceFileHasSingleLink(after) {
		return nil, ErrInvalidPriceEvidence
	}
	return body, nil
}

func extractBraveSearchPrice(body []byte) (PriceMoney, error) {
	root, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return PriceMoney{}, ErrInvalidPriceEvidence
	}
	var searchCards []*html.Node
	var findCards func(*html.Node)
	findCards = func(node *html.Node) {
		if node.Type == html.ElementNode && priceNodeHidden(node) {
			return
		}
		if node.Type == html.ElementNode && priceNodeHasClass(node, "card") && priceNodeHasClass(node, "plan") &&
			priceHeadingCount(node, "Search") == 1 {
			searchCards = append(searchCards, node)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			findCards(child)
		}
	}
	findCards(root)
	if len(searchCards) != 1 {
		return PriceMoney{}, ErrInvalidPriceEvidence
	}
	cardTokens := priceTextTokens(searchCards[0])
	priceItems := priceNodesWithClass(searchCards[0], "price-item")
	nonemptyItems := make([]*html.Node, 0, len(priceItems))
	for _, item := range priceItems {
		if len(priceTextTokens(item)) > 0 {
			nonemptyItems = append(nonemptyItems, item)
		}
	}
	if len(nonemptyItems) == 1 && strings.Join(priceTextTokens(nonemptyItems[0]), " ") == "$ 5.00 per 1,000 requests" &&
		priceCurrencyTokenCount(cardTokens) == 1 && priceTokenSequenceCount(cardTokens, []string{"per", "1,000", "requests"}) == 1 {
		return PriceMoney{Units: 5}, nil
	}
	return PriceMoney{}, ErrInvalidPriceEvidence
}

func priceTextTokens(root *html.Node) []string {
	text := make([]string, 0, 32)
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "script", "style", "template", "noscript":
				return
			}
			if priceNodeHidden(node) {
				return
			}
		}
		if node.Type == html.TextNode {
			for _, value := range strings.Fields(node.Data) {
				text = append(text, value)
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return text
}

func priceNodeHidden(node *html.Node) bool {
	for _, attribute := range node.Attr {
		switch attribute.Key {
		case "hidden":
			return true
		case "aria-hidden":
			if attribute.Val == "true" {
				return true
			}
		case "style":
			style := strings.ToLower(strings.ReplaceAll(attribute.Val, " ", ""))
			if strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden") {
				return true
			}
		}
	}
	return false
}

func priceNodeHasClass(node *html.Node, expected string) bool {
	for _, attribute := range node.Attr {
		if attribute.Key != "class" {
			continue
		}
		for _, className := range strings.Fields(attribute.Val) {
			if className == expected {
				return true
			}
		}
	}
	return false
}

func priceNodesWithClass(root *html.Node, expected string) []*html.Node {
	nodes := make([]*html.Node, 0, 4)
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && priceNodeHasClass(node, expected) {
			nodes = append(nodes, node)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return nodes
}

func priceCurrencyTokenCount(tokens []string) int {
	count := 0
	for _, token := range tokens {
		if token == "$" || strings.HasPrefix(token, "$") {
			count++
		}
	}
	return count
}

func priceHeadingCount(root *html.Node, expected string) int {
	count := 0
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "h2" && strings.Join(priceTextTokens(node), " ") == expected {
			count++
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return count
}

func priceTokenSequenceCount(tokens, sequence []string) int {
	count := 0
	for start := 0; start+len(sequence) <= len(tokens); start++ {
		matched := true
		for offset, expected := range sequence {
			if tokens[start+offset] != expected {
				matched = false
				break
			}
		}
		if matched {
			count++
		}
	}
	return count
}

func priceFileHasSingleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}

func priceDirectoryInventory(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, ErrInvalidPriceEvidence
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	sort.Strings(names)
	return names, nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func clonePriceManifest(source PriceEvidenceManifest) PriceEvidenceManifest {
	cloned := source
	cloned.Sources = append([]PriceEvidenceSource(nil), source.Sources...)
	return cloned
}
