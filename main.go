// AI Auto-Tagging plugin for Navidrome.
//
// Scheduler wiring, a kvstore-backed high-water mark, reading tracks via
// SubsonicAPI's search3, batching them through the task queue, and
// classifying each batch with the configured AI provider (Anthropic, OpenAI,
// or Gemini - see providers.go) before writing tags back via setUserTag.view.
//
// Build with:
//
//	tinygo build -o plugin.wasm -target wasip1 -buildmode=c-shared .
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/lifecycle"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"
	"github.com/navidrome/navidrome/plugins/pdk/go/scheduler"
	"github.com/navidrome/navidrome/plugins/pdk/go/taskworker"
)

const (
	scanScheduleID = "ai-auto-tagging-scan"
	classifyQueue  = "ai-auto-tagging-classify"
	offsetKey      = "scan:offset"
	searchPageSize = 100

	defaultCron       = "0 3 * * *"
	defaultLibUser    = "admin"
	defaultBatchSize  = 50
	defaultMaxPerScan = 500
	defaultProvider   = "anthropic"
	defaultModel      = "claude-haiku-4-5"
)

var defaultTagCategories = []string{"genre", "mood", "language"}

func init() {
	p := &plugin{}
	lifecycle.Register(p)
	scheduler.Register(p)
	taskworker.Register(p)
}

type plugin struct{}

func (p *plugin) OnInit() error {
	// BackoffMs starts high (and doubles per retry) because provider rate-limit
	// windows are typically ~60s - the default 1s backoff burns through all
	// retries in a few seconds without ever waiting out the window.
	if err := host.TaskCreateQueue(classifyQueue, host.QueueConfig{
		Concurrency: 2, MaxRetries: 3, BackoffMs: 60_000,
	}); err != nil {
		// Benign if the queue already exists from a prior load.
		pdk.Log(pdk.LogDebug, fmt.Sprintf("AI Auto-Tagging: task queue not (re)created: %v", err))
	}

	cron := configString("cron", defaultCron)
	if _, err := host.SchedulerScheduleRecurring(cron, "", scanScheduleID); err != nil {
		return fmt.Errorf("scheduling scan: %w", err)
	}
	pdk.Log(pdk.LogInfo, fmt.Sprintf("AI Auto-Tagging: scheduled scan with cron %q", cron))
	return nil
}

func (p *plugin) OnCallback(req scheduler.SchedulerCallbackRequest) error {
	if req.ScheduleID != scanScheduleID {
		return nil
	}
	return runScan()
}

func (p *plugin) OnTaskExecute(req taskworker.TaskExecuteRequest) (string, error) {
	var batch classifyBatch
	if err := json.Unmarshal(req.Payload, &batch); err != nil {
		return "", fmt.Errorf("decoding batch payload: %w", err)
	}
	if len(batch.Tracks) == 0 {
		return "empty batch", nil
	}

	providerName := configString("provider", defaultProvider)
	apiKey := configString("apiKey", "")
	if apiKey == "" {
		return "", fmt.Errorf("no apiKey configured for provider %q", providerName)
	}
	model := configString("model", defaultModel)
	categories := configTagCategories()
	libUser := configString("libraryUser", defaultLibUser)

	c, err := newClassifier(providerName, apiKey, model)
	if err != nil {
		return "", err
	}

	tagsByTrack, err := c.Classify(batch.Tracks, categories)
	if err != nil {
		return "", fmt.Errorf("classifying batch: %w", err)
	}

	written := 0
	for _, t := range batch.Tracks {
		for _, tag := range tagsByTrack[t.ID] {
			if err := writeUserTag(libUser, t.ID, tag); err != nil {
				return "", fmt.Errorf("writing tag %q for track %s: %w", tag, t.ID, err)
			}
			written++
		}
	}

	result := fmt.Sprintf("classified %d track(s), wrote %d tag(s)", len(batch.Tracks), written)
	pdk.Log(pdk.LogInfo, fmt.Sprintf("AI Auto-Tagging: %s (provider=%s)", result, providerName))
	return result, nil
}

func writeUserTag(user, trackID, tag string) error {
	uri := fmt.Sprintf("setUserTag?u=%s&id=%s&tag=%s",
		url.QueryEscape(user), url.QueryEscape(trackID), url.QueryEscape(tag))
	_, err := host.SubsonicAPICall(uri)
	return err
}

type trackInfo struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album,omitempty"`
}

type classifyBatch struct {
	Tracks []trackInfo `json:"tracks"`
}

type subsonicSong struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album"`
}

type subsonicEnvelope struct {
	SubsonicResponse struct {
		SearchResult3 struct {
			Song []subsonicSong `json:"song"`
		} `json:"searchResult3"`
		UserTags struct {
			Tag []string `json:"tag"`
		} `json:"userTags"`
	} `json:"subsonic-response"`
}

// runScan pages through the library starting at the stored high-water mark,
// skips tracks that already carry a user tag (best-effort - see alreadyTagged),
// and enqueues the rest in batches for later classification.
func runScan() error {
	libUser := configString("libraryUser", defaultLibUser)
	batchSize := int(configInt("batchSize", defaultBatchSize))
	maxTracks := int(configInt("maxTracksPerRun", defaultMaxPerScan))

	offset, err := loadOffset()
	if err != nil {
		return fmt.Errorf("loading scan offset: %w", err)
	}

	var pending []trackInfo
	scanned, enqueued := 0, 0

	for scanned < maxTracks {
		pageSize := searchPageSize
		if remaining := maxTracks - scanned; remaining < pageSize {
			pageSize = remaining
		}

		tracks, err := searchTracks(libUser, offset, pageSize)
		if err != nil {
			return fmt.Errorf("reading tracks at offset %d: %w", offset, err)
		}
		if len(tracks) == 0 {
			// Reached the end of the library - wrap around on the next run.
			offset = 0
			if err := saveOffset(offset); err != nil {
				return fmt.Errorf("resetting scan offset: %w", err)
			}
			break
		}

		for _, t := range tracks {
			if alreadyTagged(libUser, t.ID) {
				continue
			}
			pending = append(pending, t)
			if len(pending) >= batchSize {
				if err := enqueueBatch(pending); err != nil {
					return err
				}
				enqueued += len(pending)
				pending = nil
			}
		}

		offset += len(tracks)
		scanned += len(tracks)

		if err := saveOffset(offset); err != nil {
			return fmt.Errorf("saving scan offset: %w", err)
		}
	}

	if len(pending) > 0 {
		if err := enqueueBatch(pending); err != nil {
			return err
		}
		enqueued += len(pending)
	}

	pdk.Log(pdk.LogInfo, fmt.Sprintf(
		"AI Auto-Tagging: scan complete - %d track(s) scanned, %d enqueued for classification", scanned, enqueued))
	return nil
}

func searchTracks(user string, offset, count int) ([]trackInfo, error) {
	uri := fmt.Sprintf("search3?query=&songCount=%d&songOffset=%d&artistCount=0&albumCount=0&u=%s",
		count, offset, url.QueryEscape(user))
	respJSON, err := host.SubsonicAPICall(uri)
	if err != nil {
		return nil, err
	}
	var resp subsonicEnvelope
	if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
		return nil, fmt.Errorf("parsing search3 response: %w", err)
	}
	tracks := make([]trackInfo, 0, len(resp.SubsonicResponse.SearchResult3.Song))
	for _, s := range resp.SubsonicResponse.SearchResult3.Song {
		tracks = append(tracks, trackInfo{ID: s.ID, Title: s.Title, Artist: s.Artist, Album: s.Album})
	}
	return tracks, nil
}

// alreadyTagged reports whether a track already has a user tag, so scheduled
// re-runs don't re-classify it. getUserTags.view is a navidrome-experimental
// prerequisite (see PLAN.md); if it's unavailable, tracks are treated as
// untagged rather than failing the whole scan.
func alreadyTagged(user, trackID string) bool {
	uri := fmt.Sprintf("getUserTags?u=%s&id=%s", url.QueryEscape(user), url.QueryEscape(trackID))
	respJSON, err := host.SubsonicAPICall(uri)
	if err != nil {
		pdk.Log(pdk.LogDebug, fmt.Sprintf(
			"AI Auto-Tagging: getUserTags unavailable (%v); treating track %s as untagged", err, trackID))
		return false
	}
	var resp subsonicEnvelope
	if err := json.Unmarshal([]byte(respJSON), &resp); err != nil {
		return false
	}
	return len(resp.SubsonicResponse.UserTags.Tag) > 0
}

func enqueueBatch(tracks []trackInfo) error {
	payload, err := json.Marshal(classifyBatch{Tracks: tracks})
	if err != nil {
		return fmt.Errorf("encoding batch payload: %w", err)
	}
	if _, err := host.TaskEnqueue(classifyQueue, payload); err != nil {
		return fmt.Errorf("enqueueing batch: %w", err)
	}
	return nil
}

func loadOffset() (int, error) {
	raw, exists, err := host.KVStoreGet(offsetKey)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	n, err := strconv.Atoi(string(raw))
	if err != nil {
		return 0, nil // corrupt value; restart from the beginning
	}
	return n, nil
}

func saveOffset(offset int) error {
	return host.KVStoreSet(offsetKey, []byte(strconv.Itoa(offset)))
}

func configString(key, def string) string {
	if v, ok := host.ConfigGet(key); ok && v != "" {
		return v
	}
	return def
}

func configInt(key string, def int64) int64 {
	if v, ok := host.ConfigGetInt(key); ok {
		return v
	}
	return def
}

// configTagCategories reads the "tagCategories" config value. Array-typed
// config fields are serialized as JSON strings by the plugin host, so the
// raw value is a JSON array (e.g. `["genre","mood"]`), not a plain string.
func configTagCategories() []string {
	raw, ok := host.ConfigGet("tagCategories")
	if !ok || raw == "" {
		return defaultTagCategories
	}
	var categories []string
	if err := json.Unmarshal([]byte(raw), &categories); err != nil || len(categories) == 0 {
		return defaultTagCategories
	}
	return categories
}

func main() {}
