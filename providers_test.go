package main

import (
	"testing"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestNewClassifier_UnknownProvider(t *testing.T) {
	_, err := newClassifier("bogus", "key", "model")
	require.Error(t, err)
}

func TestNewClassifier_KnownProviders(t *testing.T) {
	for _, name := range []string{"anthropic", "openai", "gemini"} {
		c, err := newClassifier(name, "key", "model")
		require.NoError(t, err)
		require.NotNil(t, c)
	}
}

func TestParseClassifyResponse_StripsMarkdownFence(t *testing.T) {
	raw := "```json\n{\"t1\":[\"genre:rock\"]}\n```"
	got, err := parseClassifyResponse(raw)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"t1": {"genre:rock"}}, got)
}

func TestParseClassifyResponse_InvalidJSON(t *testing.T) {
	_, err := parseClassifyResponse("not json")
	require.Error(t, err)
}

func TestBuildClassifyPrompt_IncludesTrackIDsAndCategories(t *testing.T) {
	prompt := buildClassifyPrompt([]trackInfo{
		{ID: "t1", Artist: "Artist A", Title: "Title A"},
	}, []string{"genre", "mood"})

	require.Contains(t, prompt, "genre, mood")
	require.Contains(t, prompt, "id=t1")
	require.Contains(t, prompt, "Artist A")
	require.Contains(t, prompt, "Title A")
}

func TestBuildClassifyPrompt_ListsFixedVocabularyForGenreAndMood(t *testing.T) {
	prompt := buildClassifyPrompt([]trackInfo{{ID: "t1"}}, []string{"genre", "mood", "language"})

	require.Contains(t, prompt, "For genre, choose only from this list:")
	require.Contains(t, prompt, "rock")
	require.Contains(t, prompt, "For mood, choose only from this list:")
	require.Contains(t, prompt, "chill")
	require.Contains(t, prompt, "For language, use the track's actual sung/spoken language")
}

func TestParseClassifyResponse_DropsTagsOutsideFixedVocabulary(t *testing.T) {
	raw := `{"t1":["genre:rock","genre:made-up-genre","mood:chill","mood:not-a-real-mood","language:french"]}`
	got, err := parseClassifyResponse(raw)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"t1": {"genre:rock", "mood:chill", "language:french"}}, got)
}

func TestParseClassifyResponse_DropsTrackEntirelyIfAllTagsInvalid(t *testing.T) {
	raw := `{"t1":["genre:not-real"],"t2":["genre:rock"]}`
	got, err := parseClassifyResponse(raw)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"t2": {"genre:rock"}}, got)
}

func TestVocabularyFor_UnknownCategoryIsOpen(t *testing.T) {
	require.Nil(t, vocabularyFor("language"))
	require.Nil(t, vocabularyFor("bogus"))
	require.NotEmpty(t, vocabularyFor("genre"))
	require.NotEmpty(t, vocabularyFor("mood"))
}

func TestOpenAIAdapter_Classify(t *testing.T) {
	resetMocks()

	body := `{"choices":[{"message":{"content":"{\"t1\":[\"mood:chill\"]}"}}]}`
	host.HTTPMock.On("Send", mock.MatchedBy(func(req host.HTTPRequest) bool {
		return req.Method == "POST" && req.URL == openAIAPIURL && req.Headers["Authorization"] == "Bearer key"
	})).Return(&host.HTTPResponse{StatusCode: 200, Body: []byte(body)}, nil).Once()

	a := &openAIAdapter{apiKey: "key", model: "gpt-5-mini"}
	got, err := a.Classify([]trackInfo{{ID: "t1", Artist: "A", Title: "T"}}, []string{"mood"})

	require.NoError(t, err)
	require.Equal(t, map[string][]string{"t1": {"mood:chill"}}, got)
	host.HTTPMock.AssertExpectations(t)
}

func TestOpenAIAdapter_Classify_NonOKStatus(t *testing.T) {
	resetMocks()

	host.HTTPMock.On("Send", mock.Anything).
		Return(&host.HTTPResponse{StatusCode: 401, Body: []byte(`{"error":"unauthorized"}`)}, nil).Once()

	a := &openAIAdapter{apiKey: "bad-key", model: "gpt-5-mini"}
	_, err := a.Classify([]trackInfo{{ID: "t1"}}, []string{"genre"})

	require.Error(t, err)
}

func TestGeminiAdapter_Classify(t *testing.T) {
	resetMocks()

	body := `{"candidates":[{"content":{"parts":[{"text":"{\"t1\":[\"language:english\"]}"}]}}]}`
	host.HTTPMock.On("Send", mock.MatchedBy(func(req host.HTTPRequest) bool {
		return req.Method == "POST" &&
			req.URL == geminiAPIBaseURL+"/gemini-flash:generateContent?key=key"
	})).Return(&host.HTTPResponse{StatusCode: 200, Body: []byte(body)}, nil).Once()

	a := &geminiAdapter{apiKey: "key", model: "gemini-flash"}
	got, err := a.Classify([]trackInfo{{ID: "t1", Artist: "A", Title: "T"}}, []string{"language"})

	require.NoError(t, err)
	require.Equal(t, map[string][]string{"t1": {"language:english"}}, got)
	host.HTTPMock.AssertExpectations(t)
}

func TestAnthropicAdapter_Classify(t *testing.T) {
	resetMocks()

	body := `{"content":[{"type":"text","text":"{\"t1\":[\"genre:rock\"]}"}]}`
	host.HTTPMock.On("Send", mock.MatchedBy(func(req host.HTTPRequest) bool {
		return req.Method == "POST" && req.URL == anthropicAPIURL &&
			req.Headers["x-api-key"] == "key" && req.Headers["anthropic-version"] == anthropicVersion
	})).Return(&host.HTTPResponse{StatusCode: 200, Body: []byte(body)}, nil).Once()

	a := &anthropicAdapter{apiKey: "key", model: "claude-haiku-4-5"}
	got, err := a.Classify([]trackInfo{{ID: "t1", Artist: "A", Title: "T"}}, []string{"genre"})

	require.NoError(t, err)
	require.Equal(t, map[string][]string{"t1": {"genre:rock"}}, got)
	host.HTTPMock.AssertExpectations(t)
}
