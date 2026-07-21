package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
)

// genreVocabulary and moodVocabulary constrain the model to a fixed, small
// set of values per category. Without this, open-ended word choice tends to
// fragment into near-duplicates across batches (e.g. "chill"/"relaxed"/
// "mellow" for the same concept) - a problem for anything building one
// playlist per discovered tag value (see ai-mood-playlists). language is
// deliberately left open-vocabulary since it should reflect the track's
// actual language, not a curated list.
var genreVocabulary = []string{
	"rock", "pop", "electronic", "hip hop", "jazz", "classical", "metal",
	"folk", "country", "r&b", "soul", "blues", "reggae", "punk", "indie",
	"ambient", "new age", "world", "funk", "disco", "house", "techno",
	"alternative", "soundtrack", "experimental",
}

var moodVocabulary = []string{
	"happy", "chill", "energetic", "melancholy", "party", "aggressive",
	"romantic", "dreamy", "dark", "uplifting", "nostalgic", "peaceful",
}

// vocabularyFor returns the fixed set of allowed values for a category, or
// nil if the category is open-vocabulary (e.g. language).
func vocabularyFor(category string) []string {
	switch category {
	case "genre":
		return genreVocabulary
	case "mood":
		return moodVocabulary
	default:
		return nil
	}
}

// classifier is implemented by each AI provider adapter. Classify returns a
// map from track ID to suggested tags (each tag prefixed with its category,
// e.g. "genre:rock", to avoid collisions across categories in the shared
// freeform media_file_tag namespace).
type classifier interface {
	Classify(tracks []trackInfo, categories []string) (map[string][]string, error)
}

func newClassifier(providerName, apiKey, model string) (classifier, error) {
	switch providerName {
	case "anthropic":
		return &anthropicAdapter{apiKey: apiKey, model: model}, nil
	case "openai":
		return &openAIAdapter{apiKey: apiKey, model: model}, nil
	case "gemini":
		return &geminiAdapter{apiKey: apiKey, model: model}, nil
	default:
		return nil, fmt.Errorf("unknown provider %q (expected anthropic, openai, or gemini)", providerName)
	}
}

// buildClassifyPrompt is shared across providers: each adapter only differs
// in request/response transport, not in what's asked of the model.
func buildClassifyPrompt(tracks []trackInfo, categories []string) string {
	var b strings.Builder
	b.WriteString("You are a music classification assistant. For each track below, suggest tags for these categories: ")
	b.WriteString(strings.Join(categories, ", "))
	b.WriteString(".\n\n")
	for _, category := range categories {
		switch {
		case len(vocabularyFor(category)) > 0:
			fmt.Fprintf(&b, "For %s, choose only from this list: %s.\n",
				category, strings.Join(vocabularyFor(category), ", "))
		case category == "language":
			b.WriteString("For language, use the track's actual sung/spoken language (e.g. \"english\", " +
				"\"french\"), or \"instrumental\" if there are no vocals.\n")
		}
	}
	b.WriteString("\n")
	b.WriteString("Respond with ONLY a JSON object (no markdown, no explanation) mapping each track's id to an array of tag strings. ")
	b.WriteString("Prefix every tag with its category, e.g. \"genre:rock\", \"mood:energetic\", \"language:english\". ")
	b.WriteString("Suggest 1-3 tags per category per track. Use lowercase values.\n\n")
	b.WriteString("Tracks:\n")
	for _, t := range tracks {
		fmt.Fprintf(&b, "- id=%s artist=%q title=%q", t.ID, t.Artist, t.Title)
		if t.Album != "" {
			fmt.Fprintf(&b, " album=%q", t.Album)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nJSON:")
	return b.String()
}

// parseClassifyResponse parses the model's reply text as the requested
// track-id -> tags JSON object, stripping a markdown code fence if the model
// added one despite being told not to.
func parseClassifyResponse(raw string) (map[string][]string, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var result map[string][]string
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, fmt.Errorf("parsing model response as JSON: %w", err)
	}
	return filterToVocabulary(result), nil
}

// filterToVocabulary drops any tag whose category has a fixed vocabulary
// (see vocabularyFor) but whose value isn't in it, guarding against the
// model occasionally ignoring the vocabulary instruction. Tags in
// categories without a fixed vocabulary (e.g. language) pass through
// unchanged.
func filterToVocabulary(tagsByTrack map[string][]string) map[string][]string {
	filtered := make(map[string][]string, len(tagsByTrack))
	for trackID, tags := range tagsByTrack {
		var kept []string
		for _, tag := range tags {
			category, value, ok := strings.Cut(tag, ":")
			if !ok {
				kept = append(kept, tag)
				continue
			}
			if vocab := vocabularyFor(category); len(vocab) == 0 || slices.Contains(vocab, value) {
				kept = append(kept, tag)
			}
		}
		if len(kept) > 0 {
			filtered[trackID] = kept
		}
	}
	return filtered
}

const httpTimeoutMs = 30000

// --- Anthropic ---

const anthropicAPIURL = "https://api.anthropic.com/v1/messages"
const anthropicVersion = "2023-06-01"

type anthropicAdapter struct {
	apiKey string
	model  string
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (a *anthropicAdapter) Classify(tracks []trackInfo, categories []string) (map[string][]string, error) {
	reqBody, err := json.Marshal(anthropicRequest{
		Model:     a.model,
		MaxTokens: 4096,
		Messages:  []anthropicMessage{{Role: "user", Content: buildClassifyPrompt(tracks, categories)}},
	})
	if err != nil {
		return nil, err
	}

	resp, err := host.HTTPSend(host.HTTPRequest{
		Method: "POST",
		URL:    anthropicAPIURL,
		Headers: map[string]string{
			"x-api-key":         a.apiKey,
			"anthropic-version": anthropicVersion,
			"content-type":      "application/json",
		},
		Body:      reqBody,
		TimeoutMs: httpTimeoutMs,
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic HTTP error: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("anthropic API error: status %d: %s", resp.StatusCode, string(resp.Body))
	}

	var apiResp anthropicResponse
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return nil, fmt.Errorf("parsing anthropic response: %w", err)
	}
	if len(apiResp.Content) == 0 {
		return nil, errors.New("anthropic response had no content")
	}
	return parseClassifyResponse(apiResp.Content[0].Text)
}

// --- OpenAI ---

const openAIAPIURL = "https://api.openai.com/v1/chat/completions"

type openAIAdapter struct {
	apiKey string
	model  string
}

type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (a *openAIAdapter) Classify(tracks []trackInfo, categories []string) (map[string][]string, error) {
	reqBody, err := json.Marshal(openAIRequest{
		Model:    a.model,
		Messages: []openAIMessage{{Role: "user", Content: buildClassifyPrompt(tracks, categories)}},
	})
	if err != nil {
		return nil, err
	}

	resp, err := host.HTTPSend(host.HTTPRequest{
		Method: "POST",
		URL:    openAIAPIURL,
		Headers: map[string]string{
			"Authorization": "Bearer " + a.apiKey,
			"Content-Type":  "application/json",
		},
		Body:      reqBody,
		TimeoutMs: httpTimeoutMs,
	})
	if err != nil {
		return nil, fmt.Errorf("openai HTTP error: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openai API error: status %d: %s", resp.StatusCode, string(resp.Body))
	}

	var apiResp openAIResponse
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return nil, fmt.Errorf("parsing openai response: %w", err)
	}
	if len(apiResp.Choices) == 0 {
		return nil, errors.New("openai response had no choices")
	}
	return parseClassifyResponse(apiResp.Choices[0].Message.Content)
}

// --- Gemini ---

const geminiAPIBaseURL = "https://generativelanguage.googleapis.com/v1beta/models"

type geminiAdapter struct {
	apiKey string
	model  string
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiResponse struct {
	Candidates []struct {
		Content geminiContent `json:"content"`
	} `json:"candidates"`
}

func (a *geminiAdapter) Classify(tracks []trackInfo, categories []string) (map[string][]string, error) {
	reqBody, err := json.Marshal(geminiRequest{
		Contents: []geminiContent{{Parts: []geminiPart{{Text: buildClassifyPrompt(tracks, categories)}}}},
	})
	if err != nil {
		return nil, err
	}

	resp, err := host.HTTPSend(host.HTTPRequest{
		Method:    "POST",
		URL:       fmt.Sprintf("%s/%s:generateContent?key=%s", geminiAPIBaseURL, a.model, a.apiKey),
		Headers:   map[string]string{"Content-Type": "application/json"},
		Body:      reqBody,
		TimeoutMs: httpTimeoutMs,
	})
	if err != nil {
		return nil, fmt.Errorf("gemini HTTP error: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gemini API error: status %d: %s", resp.StatusCode, string(resp.Body))
	}

	var apiResp geminiResponse
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return nil, fmt.Errorf("parsing gemini response: %w", err)
	}
	if len(apiResp.Candidates) == 0 || len(apiResp.Candidates[0].Content.Parts) == 0 {
		return nil, errors.New("gemini response had no candidates")
	}
	return parseClassifyResponse(apiResp.Candidates[0].Content.Parts[0].Text)
}
