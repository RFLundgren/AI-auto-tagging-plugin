package main

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/navidrome/navidrome/plugins/pdk/go/host"
	"github.com/navidrome/navidrome/plugins/pdk/go/pdk"
	"github.com/navidrome/navidrome/plugins/pdk/go/scheduler"
	"github.com/navidrome/navidrome/plugins/pdk/go/taskworker"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// resetMocks clears expectations/calls recorded on the shared PDK mock
// instances so each test starts from a clean slate.
func resetMocks() {
	host.ConfigMock.ExpectedCalls, host.ConfigMock.Calls = nil, nil
	host.KVStoreMock.ExpectedCalls, host.KVStoreMock.Calls = nil, nil
	host.SchedulerMock.ExpectedCalls, host.SchedulerMock.Calls = nil, nil
	host.SubsonicAPIMock.ExpectedCalls, host.SubsonicAPIMock.Calls = nil, nil
	host.TaskMock.ExpectedCalls, host.TaskMock.Calls = nil, nil
	host.HTTPMock.ExpectedCalls, host.HTTPMock.Calls = nil, nil
	pdk.PDKMock.ExpectedCalls, pdk.PDKMock.Calls = nil, nil
	pdk.PDKMock.On("Log", mock.Anything, mock.Anything).Return().Maybe()
}

func TestOnInit_CreatesQueueAndSchedulesScan(t *testing.T) {
	resetMocks()

	host.TaskMock.On("CreateQueue", classifyQueue, host.QueueConfig{Concurrency: 2, MaxRetries: 3, BackoffMs: 60_000}).
		Return(nil).Once()
	host.ConfigMock.On("Get", "cron").Return("0 3 * * *", true).Once()
	host.SchedulerMock.On("ScheduleRecurring", "0 3 * * *", "", scanScheduleID).
		Return(scanScheduleID, nil).Once()

	err := (&plugin{}).OnInit()

	require.NoError(t, err)
	host.TaskMock.AssertExpectations(t)
	host.SchedulerMock.AssertExpectations(t)
}

func TestOnInit_PropagatesScheduleError(t *testing.T) {
	resetMocks()

	host.TaskMock.On("CreateQueue", classifyQueue, host.QueueConfig{Concurrency: 2, MaxRetries: 3, BackoffMs: 60_000}).
		Return(nil).Once()
	host.ConfigMock.On("Get", "cron").Return("", false).Once()
	host.SchedulerMock.On("ScheduleRecurring", defaultCron, "", scanScheduleID).
		Return("", errors.New("scheduler unavailable")).Once()

	err := (&plugin{}).OnInit()

	require.Error(t, err)
}

func TestRunScan_SkipsTaggedTracksAndBatchesTheRest(t *testing.T) {
	resetMocks()

	host.ConfigMock.On("Get", "libraryUser").Return("admin", true).Once()
	host.ConfigMock.On("GetInt", "batchSize").Return(int64(2), true).Once()
	host.ConfigMock.On("GetInt", "maxTracksPerRun").Return(int64(3), true).Once()
	host.KVStoreMock.On("Get", offsetKey).Return([]byte{}, false, nil).Once()

	searchResp := `{"subsonic-response":{"searchResult3":{"song":[
		{"id":"t1","title":"Song One","artist":"Artist A","album":"Album X"},
		{"id":"t2","title":"Song Two","artist":"Artist B","album":"Album Y"},
		{"id":"t3","title":"Song Three","artist":"Artist C","album":"Album Z"}
	]}}}`
	host.SubsonicAPIMock.On("Call", "search3?query=&songCount=3&songOffset=0&artistCount=0&albumCount=0&u=admin").
		Return(searchResp, nil).Once()

	host.SubsonicAPIMock.On("Call", "getUserTags?u=admin&id=t1").
		Return(`{"subsonic-response":{"userTags":{"tag":["mood:chill"]}}}`, nil).Once()
	host.SubsonicAPIMock.On("Call", "getUserTags?u=admin&id=t2").
		Return(`{"subsonic-response":{"userTags":{"tag":[]}}}`, nil).Once()
	host.SubsonicAPIMock.On("Call", "getUserTags?u=admin&id=t3").
		Return(`{"subsonic-response":{"userTags":{"tag":[]}}}`, nil).Once()

	host.TaskMock.On("Enqueue", classifyQueue, mock.MatchedBy(func(payload []byte) bool {
		var batch classifyBatch
		if err := json.Unmarshal(payload, &batch); err != nil {
			return false
		}
		if len(batch.Tracks) != 2 {
			return false
		}
		return batch.Tracks[0].ID == "t2" && batch.Tracks[1].ID == "t3"
	})).Return("task-1", nil).Once()

	host.KVStoreMock.On("Set", offsetKey, []byte("3")).Return(nil).Once()

	err := runScan()

	require.NoError(t, err)
	host.SubsonicAPIMock.AssertExpectations(t)
	host.TaskMock.AssertExpectations(t)
	host.KVStoreMock.AssertExpectations(t)
}

func TestOnTaskExecute_ClassifiesWithDefaultProviderAndWritesTags(t *testing.T) {
	resetMocks()

	host.ConfigMock.On("Get", "provider").Return("", false).Once()
	host.ConfigMock.On("Get", "apiKey").Return("test-key", true).Once()
	host.ConfigMock.On("Get", "model").Return("", false).Once()
	host.ConfigMock.On("Get", "tagCategories").Return("", false).Once()
	host.ConfigMock.On("Get", "libraryUser").Return("", false).Once()

	anthropicBody := `{"content":[{"type":"text","text":` +
		`"{\"t2\":[\"genre:rock\",\"mood:energetic\"],\"t3\":[\"language:english\"]}"}]}`
	host.HTTPMock.On("Send", mock.MatchedBy(func(req host.HTTPRequest) bool {
		if req.Method != "POST" || req.URL != anthropicAPIURL {
			return false
		}
		if req.Headers["x-api-key"] != "test-key" {
			return false
		}
		var body anthropicRequest
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return false
		}
		return body.Model == defaultModel && len(body.Messages) == 1
	})).Return(&host.HTTPResponse{StatusCode: 200, Body: []byte(anthropicBody)}, nil).Once()

	host.SubsonicAPIMock.On("Call", "setUserTag?u=admin&id=t2&tag=genre%3Arock").Return("", nil).Once()
	host.SubsonicAPIMock.On("Call", "setUserTag?u=admin&id=t2&tag=mood%3Aenergetic").Return("", nil).Once()
	host.SubsonicAPIMock.On("Call", "setUserTag?u=admin&id=t3&tag=language%3Aenglish").Return("", nil).Once()

	payload, err := json.Marshal(classifyBatch{Tracks: []trackInfo{
		{ID: "t2", Title: "Song Two", Artist: "Artist B"},
		{ID: "t3", Title: "Song Three", Artist: "Artist C"},
	}})
	require.NoError(t, err)

	result, err := (&plugin{}).OnTaskExecute(taskworker.TaskExecuteRequest{
		QueueName: classifyQueue,
		TaskID:    "task-1",
		Payload:   payload,
		Attempt:   1,
	})

	require.NoError(t, err)
	require.Equal(t, "classified 2 track(s), wrote 3 tag(s)", result)
	host.HTTPMock.AssertExpectations(t)
	host.SubsonicAPIMock.AssertExpectations(t)
}

func TestOnTaskExecute_ErrorsWithoutAPIKey(t *testing.T) {
	resetMocks()

	host.ConfigMock.On("Get", "provider").Return("", false).Once()
	host.ConfigMock.On("Get", "apiKey").Return("", false).Once()

	payload, err := json.Marshal(classifyBatch{Tracks: []trackInfo{{ID: "t1"}}})
	require.NoError(t, err)

	_, err = (&plugin{}).OnTaskExecute(taskworker.TaskExecuteRequest{Payload: payload})

	require.Error(t, err)
	host.HTTPMock.AssertNotCalled(t, "Send", mock.Anything)
}

func TestOnCallback_IgnoresOtherSchedules(t *testing.T) {
	resetMocks()

	err := (&plugin{}).OnCallback(scheduler.SchedulerCallbackRequest{ScheduleID: "some-other-schedule"})

	require.NoError(t, err)
	host.SubsonicAPIMock.AssertNotCalled(t, "Call", mock.Anything)
}
