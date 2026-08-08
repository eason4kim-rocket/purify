package quality

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/models"
)

func TestStatusForScoreBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		score float64
		want  models.QualityStatus
	}{
		{name: "below zero", score: -0.1, want: models.QualityStatusUnusable},
		{name: "below usable boundary", score: 0.699999, want: models.QualityStatusUnusable},
		{name: "usable boundary inclusive", score: 0.70, want: models.QualityStatusDegraded},
		{name: "below good boundary", score: 0.849999, want: models.QualityStatusDegraded},
		{name: "good boundary inclusive", score: 0.85, want: models.QualityStatusGood},
		{name: "perfect", score: 1, want: models.QualityStatusGood},
		{name: "above one", score: 1.1, want: models.QualityStatusGood},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StatusForScore(tt.score); got != tt.want {
				t.Fatalf("StatusForScore(%v) = %q, want %q", tt.score, got, tt.want)
			}
		})
	}
}

func TestEvaluateCandidateHardFailureMatrix(t *testing.T) {
	longCleaned := strings.Repeat("useful article content ", 100)
	tests := []struct {
		name    string
		rawHTML string
		cleaned string
		want    models.QualityReason
	}{
		{
			name:    "missing body",
			rawHTML: `<html><head><title>Head only</title></head></html>`,
			cleaned: longCleaned,
			want:    models.QualityReasonMissingBody,
		},
		{
			name:    "empty body ignores scripts styles and templates",
			rawHTML: `<html><body><script>document.write("late")</script><style>body{}</style><template>placeholder</template></body></html>`,
			cleaned: longCleaned,
			want:    models.QualityReasonEmptyContent,
		},
		{
			name:    "loading placeholder",
			rawHTML: `<html><body><main aria-busy="true">Loading content...</main></body></html>`,
			cleaned: `Loading content...`,
			want:    models.QualityReasonLoadingPage,
		},
		{
			name:    "browser error page",
			rawHTML: `<html><body><main id="main-frame-error">This site can't be reached ERR_NAME_NOT_RESOLVED</main></body></html>`,
			cleaned: `This site can't be reached ERR_NAME_NOT_RESOLVED`,
			want:    models.QualityReasonErrorPage,
		},
		{
			name:    "challenge substituted for target",
			rawHTML: `<html><body><div id="cf-chl-widget">Checking your browser before accessing the site</div></body></html>`,
			cleaned: `Checking your browser before accessing the site`,
			want:    models.QualityReasonChallengePage,
		},
		{
			name:    "cleaner returned empty content",
			rawHTML: `<html><body><article>The fetched document has a real body.</article></body></html>`,
			cleaned: `  `,
			want:    models.QualityReasonEmptyContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateCandidate(Candidate{
				RawHTML:        tt.rawHTML,
				CleanedContent: tt.cleaned,
				ExtractMode:    "readability",
			})
			if !got.HardFailure {
				t.Fatalf("HardFailure = false, want true: %#v", got)
			}
			if got.Usable() {
				t.Fatalf("Usable() = true, want false: %#v", got)
			}
			if got.Reason != tt.want || got.RejectionReason() != tt.want {
				t.Fatalf("reason = %q / %q, want %q", got.Reason, got.RejectionReason(), tt.want)
			}
			if got.Info.Score != 0 || got.Info.Status != models.QualityStatusUnusable {
				t.Fatalf("quality = %#v, want zero/unusable", got.Info)
			}
			if len(got.Info.Warnings) != 1 || got.Info.Warnings[0] != tt.want {
				t.Fatalf("warnings = %#v, want [%q]", got.Info.Warnings, tt.want)
			}
			if got.Info.ExtractModeUsed != "readability" {
				t.Fatalf("extract_mode_used = %q, want readability", got.Info.ExtractModeUsed)
			}

			attempt := got.AsFetchAttempt("http", 820*time.Millisecond)
			if attempt.Outcome != models.FetchAttemptRejected || attempt.Reason != tt.want || attempt.DurationMs != 820 {
				t.Fatalf("attempt = %#v", attempt)
			}

			qualityErr := got.ContentUnusableError()
			if qualityErr == nil || qualityErr.Code != models.ErrCodeContentUnusable {
				t.Fatalf("error = %#v, want code %q", qualityErr, models.ErrCodeContentUnusable)
			}
			if !strings.Contains(qualityErr.Message, string(tt.want)) {
				t.Fatalf("error message = %q, want reason %q", qualityErr.Message, tt.want)
			}
		})
	}
}

func TestEvaluateCleanedContentScoreMatrix(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		wantScore  float64
		wantStatus models.QualityStatus
		wantUsable bool
		wantHard   bool
	}{
		{
			name:       "substantial text is good",
			content:    strings.Repeat("useful ", 30),
			wantScore:  0.90,
			wantStatus: models.QualityStatusGood,
			wantUsable: true,
		},
		{
			name:       "short but meaningful text is degraded",
			content:    strings.Repeat("useful ", 6),
			wantScore:  0.75,
			wantStatus: models.QualityStatusDegraded,
			wantUsable: true,
		},
		{
			name:       "nonempty fragment below usable bar",
			content:    strings.Repeat("useful ", 3),
			wantScore:  0.65,
			wantStatus: models.QualityStatusUnusable,
			wantUsable: false,
		},
		{
			name:       "markup volume does not count as content",
			content:    `<script>` + strings.Repeat("not visible ", 100) + `</script><style>body{color:red}</style>`,
			wantScore:  0,
			wantStatus: models.QualityStatusUnusable,
			wantUsable: false,
			wantHard:   true,
		},
		{
			name:       "CJK text is informative",
			content:    strings.Repeat("可靠内容", 50),
			wantScore:  0.90,
			wantStatus: models.QualityStatusGood,
			wantUsable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateCleanedContent(tt.content, "pruning")
			if got.Info.Score != tt.wantScore || got.Info.Status != tt.wantStatus {
				t.Fatalf("quality = %#v, want score=%v status=%q", got.Info, tt.wantScore, tt.wantStatus)
			}
			if got.Usable() != tt.wantUsable || got.HardFailure != tt.wantHard {
				t.Fatalf("Usable/HardFailure = %v/%v, want %v/%v", got.Usable(), got.HardFailure, tt.wantUsable, tt.wantHard)
			}
			if got.Info.ExtractModeUsed != "pruning" {
				t.Fatalf("extract_mode_used = %q, want pruning", got.Info.ExtractModeUsed)
			}
			if tt.wantStatus == models.QualityStatusGood {
				if got.Info.Warnings == nil || len(got.Info.Warnings) != 0 {
					t.Fatalf("good warnings = %#v, want non-nil empty slice", got.Info.Warnings)
				}
			} else if len(got.Info.Warnings) == 0 {
				t.Fatal("non-good result must provide a warning")
			}
		})
	}
}

func TestEvaluateCleanedContentSubstitutedPageMatrix(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    models.QualityReason
	}{
		{
			name:    "loading output",
			content: "Please wait... Loading content",
			want:    models.QualityReasonLoadingPage,
		},
		{
			name:    "browser error output",
			content: "This site can’t be reached. ERR_NAME_NOT_RESOLVED",
			want:    models.QualityReasonErrorPage,
		},
		{
			name:    "challenge output",
			content: "Attention required: verify you are human to continue",
			want:    models.QualityReasonChallengePage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateCleanedContent(tt.content, "raw")
			if !got.HardFailure || got.Reason != tt.want || got.Usable() {
				t.Fatalf("assessment = %#v, want hard failure %q", got, tt.want)
			}
		})
	}
}

func TestEvaluateRawDocumentPassesCompleteBody(t *testing.T) {
	rawHTML := `<html><head><title>Title text is not body text</title></head><body><article>Actual article content.</article></body></html>`
	if reason := EvaluateRawDocument(rawHTML); reason != "" {
		t.Fatalf("EvaluateRawDocument() = %q, want pass", reason)
	}
}

func TestLoadingWordInRealContentIsNotAHardFailure(t *testing.T) {
	content := "Loading performance guide for frontend applications"
	rawHTML := `<html><body><article>` + content + `</article></body></html>`
	if reason := EvaluateRawDocument(rawHTML); reason != "" {
		t.Fatalf("EvaluateRawDocument() = %q, want no hard failure", reason)
	}
	got := EvaluateCleanedContent(content, "readability")
	if got.HardFailure || got.Reason == models.QualityReasonLoadingPage {
		t.Fatalf("real content misclassified as loading page: %#v", got)
	}
}

func TestEvaluateCandidateDoesNotRejectLongArticleQuotingChallengeLanguage(t *testing.T) {
	article := strings.Repeat("This article explains browser security checks in useful technical detail. ", 40) +
		"It quotes the phrase verify you are human while documenting challenge pages."
	rawHTML := `<html><body><article>` + article + `</article></body></html>`

	got := EvaluateCandidate(Candidate{RawHTML: rawHTML, CleanedContent: article, ExtractMode: "readability"})
	if !got.Usable() || got.HardFailure || got.Reason != "" {
		t.Fatalf("legitimate article rejected: %#v", got)
	}
	if got.ContentUnusableError() != nil {
		t.Fatal("usable result returned CONTENT_UNUSABLE")
	}
	attempt := got.AsFetchAttempt("rod", 2160*time.Millisecond)
	if attempt.Outcome != models.FetchAttemptSelected || attempt.Reason != "" || attempt.DurationMs != 2160 {
		t.Fatalf("attempt = %#v", attempt)
	}
}

func TestQualityInfoPublicJSONContract(t *testing.T) {
	assessment := EvaluateCleanedContent(strings.Repeat("useful ", 30), "pruning")
	assessment.Info.FetchAttempts = []models.FetchAttempt{
		{
			Engine:     "http",
			Outcome:    models.FetchAttemptRejected,
			Reason:     models.QualityReasonMissingBody,
			DurationMs: 820,
		},
		{
			Engine:     "rod",
			Outcome:    models.FetchAttemptSelected,
			DurationMs: 2160,
		},
	}

	encoded, err := json.Marshal(assessment.Info)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("Unmarshal() error = %v, JSON = %s", err, encoded)
	}
	for _, key := range []string{"score", "status", "warnings", "extract_mode_used", "fetch_attempts"} {
		if _, ok := document[key]; !ok {
			t.Fatalf("JSON %s missing key %q", encoded, key)
		}
	}
	warnings, ok := document["warnings"].([]any)
	if !ok || len(warnings) != 0 {
		t.Fatalf("warnings = %#v, want []", document["warnings"])
	}
	attempts, ok := document["fetch_attempts"].([]any)
	if !ok || len(attempts) != 2 {
		t.Fatalf("fetch_attempts = %#v", document["fetch_attempts"])
	}
	selected := attempts[1].(map[string]any)
	if _, present := selected["reason"]; present {
		t.Fatalf("selected attempt unexpectedly has reason: %#v", selected)
	}
}

func TestScrapeResponseOmitsNilQualityForLegacyCompatibility(t *testing.T) {
	encoded, err := json.Marshal(models.ScrapeResponse{
		Success:    true,
		StatusCode: 200,
		FinalURL:   "https://example.com/final",
		Content:    "legacy content",
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, present := document["quality"]; present {
		t.Fatalf("legacy response unexpectedly contains quality: %s", encoded)
	}
}

func TestScrapeResponseIncludesCompleteQualityContract(t *testing.T) {
	info := EvaluateCleanedContent(strings.Repeat("useful ", 30), "pruning").Info
	info.FetchAttempts = []models.FetchAttempt{
		{
			Engine:     "http",
			Outcome:    models.FetchAttemptRejected,
			Reason:     models.QualityReasonMissingBody,
			DurationMs: 820,
		},
		{
			Engine:     "rod",
			Outcome:    models.FetchAttemptSelected,
			DurationMs: 2160,
		},
	}
	encoded, err := json.Marshal(models.ScrapeResponse{
		Success:    true,
		StatusCode: 200,
		FinalURL:   "https://example.com/final",
		Content:    "clean content",
		Quality:    &info,
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	qualityBlock, ok := document["quality"].(map[string]any)
	if !ok {
		t.Fatalf("quality = %#v, JSON = %s", document["quality"], encoded)
	}
	for _, key := range []string{"score", "status", "warnings", "extract_mode_used", "fetch_attempts"} {
		if _, present := qualityBlock[key]; !present {
			t.Fatalf("quality block %#v missing %q", qualityBlock, key)
		}
	}
	warnings, ok := qualityBlock["warnings"].([]any)
	if !ok || len(warnings) != 0 {
		t.Fatalf("warnings = %#v, want []", qualityBlock["warnings"])
	}
	attempts, ok := qualityBlock["fetch_attempts"].([]any)
	if !ok || len(attempts) != 2 {
		t.Fatalf("fetch_attempts = %#v", qualityBlock["fetch_attempts"])
	}
}

func TestNewContentUnusableErrorContract(t *testing.T) {
	err := NewContentUnusableError(models.QualityReasonLowContentQuality)
	if err.Code != "CONTENT_UNUSABLE" || err.ToDetail().Code != "CONTENT_UNUSABLE" {
		t.Fatalf("error = %#v, detail = %#v", err, err.ToDetail())
	}
}
