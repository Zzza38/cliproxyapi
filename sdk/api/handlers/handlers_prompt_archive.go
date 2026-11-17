package handlers

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/promptarchive"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"golang.org/x/net/context"
)

// capturePrompt starts archiving this model call when the prompt archive is enabled.
// rawJSON is the client request as received, before plugin interceptors.
func (t *requestLifecycleTracker) capturePrompt(ctx context.Context, entryProtocol, responseProtocol string, rawJSON []byte, internal bool) {
	if t == nil || !promptarchive.Enabled() {
		return
	}
	info := promptarchive.CaptureInfo{
		Protocol:         entryProtocol,
		ResponseProtocol: responseProtocol,
		Model:            t.completion.Model,
		Requested:        t.completion.RequestedModel,
		Stream:           t.completion.Stream,
		Trace:            t.completion.TraceID,
		Internal:         internal,
		Key:              coreusage.APIKeyFromContext(ctx),
		Session:          util.SessionIDFromContext(ctx),
	}
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			path := ginCtx.FullPath()
			if path == "" && ginCtx.Request.URL != nil {
				path = ginCtx.Request.URL.Path
			}
			info.Endpoint = strings.TrimSpace(ginCtx.Request.Method + " " + path)
			info.ClientIP = ginCtx.ClientIP()
			info.UserAgent = ginCtx.GetHeader("User-Agent")
			if info.Key == "" {
				if value, exists := ginCtx.Get("userApiKey"); exists && value != nil {
					info.Key = fmt.Sprint(value)
				}
			}
		}
	}
	t.prompt = promptarchive.Begin(info, rawJSON)
	// The conductor clones request metadata, so learn the serving credential
	// through the selected-auth callback, which every clone carries along.
	if capture, meta := t.prompt, t.completion.Metadata; capture != nil && meta != nil {
		prev, _ := meta[coreexecutor.SelectedAuthCallbackMetadataKey].(func(string))
		meta[coreexecutor.SelectedAuthCallbackMetadataKey] = func(authID string) {
			capture.SetAuth(authID)
			if prev != nil {
				prev(authID)
			}
		}
	}
}

func (t *requestLifecycleTracker) capturePromptSession(ctx context.Context) {
	if t != nil {
		t.prompt.SetSession(util.SessionIDFromContext(ctx))
	}
}

func (t *requestLifecycleTracker) capturePromptChunk(chunk []byte) {
	if t != nil {
		t.prompt.AddChunk(chunk)
	}
}

func (t *requestLifecycleTracker) capturePromptResponse(body []byte) {
	if t != nil {
		t.prompt.SetResponse(body)
	}
}

func (t *requestLifecycleTracker) finishPrompt(outcome pluginapi.RequestCompletionOutcome, statusCode int, err error) {
	if t == nil || t.prompt == nil {
		return
	}
	result := string(outcome)
	if outcome == pluginapi.RequestCompletionSucceeded {
		result = "ok"
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	t.prompt.Finish(result, statusCode, errText)
}

// archiveRejectedPrompt records a call that failed before an upstream was chosen,
// such as an unknown model, so the archive still holds every prompt received.
func (h *BaseAPIHandler) archiveRejectedPrompt(ctx context.Context, entryProtocol, responseProtocol, model string, stream bool, rawJSON []byte, internal bool, errMsg *interfaces.ErrorMessage) {
	if errMsg == nil || !promptarchive.Enabled() {
		return
	}
	t := &requestLifecycleTracker{completion: pluginapi.RequestCompletion{
		Model:          model,
		RequestedModel: model,
		Stream:         stream,
		TraceID:        logging.GetRequestID(ctx),
	}}
	t.capturePrompt(ctx, entryProtocol, responseProtocol, rawJSON, internal)
	t.finishPrompt(pluginapi.RequestCompletionFailed, errMsg.StatusCode, errMsg.Error)
}
