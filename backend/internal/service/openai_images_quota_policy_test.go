//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type imageQuotaPolicyRepo struct {
	modelNotFoundAccountRepoStub
	until  time.Time
	reason string
}

func (r *imageQuotaPolicyRepo) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.tempCalls++
	r.until, r.reason = until, reason
	return nil
}

type imageTestErrorRecorder struct{ entries []*OpsInsertErrorLogInput }

func (r *imageTestErrorRecorder) RecordError(_ context.Context, entry *OpsInsertErrorLogInput) error {
	r.entries = append(r.entries, entry)
	return nil
}
func imageQuotaPolicyAccount(scope string, pool bool) *Account {
	a := newOpenAIImagesAPIKeyAccount()
	a.Status, a.Schedulable = StatusActive, true
	a.Credentials["pool_mode"] = pool
	a.Credentials["temp_unschedulable_enabled"] = true
	a.Credentials["temp_unschedulable_rules"] = []any{map[string]any{
		"error_code": 429, "duration_minutes": 10, "scope": scope,
		"keywords": []any{"rate limit", "too many requests", "no available image quota"},
	}}
	return a
}
func TestImageQuotaPolicy_AdminTestPausesAndRecordsError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, oauth := range []bool{false, true} {
		for _, wrapped := range []bool{false, true} {
			t.Run(fmtImageQuotaCase(oauth, wrapped), func(t *testing.T) {
				repo := &imageQuotaPolicyRepo{}
				gateway := &OpenAIGatewayService{rateLimitService: &RateLimitService{accountRepo: repo}}
				gateway.rateLimitService.SetAccountRuntimeBlocker(gateway)
				recorder := &imageTestErrorRecorder{}
				status := http.StatusTooManyRequests
				body := `{"error":{"message":"no available image quota"}}`
				if wrapped {
					status = http.StatusOK
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status,
					Header: http.Header{"X-Request-Id": []string{"req-quota-test"}}, Body: io.NopCloser(strings.NewReader(body))}}
				svc := &AccountTestService{cfg: &config.Config{}, httpUpstream: upstream, openaiGatewayService: gateway, opsErrorRecorder: recorder}
				a := imageQuotaPolicyAccount("account", false)
				c, _ := newOpenAIImagesTestContext(t, nil)
				c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/222/test", nil)
				var err error
				if oauth {
					a.Type = AccountTypeOAuth
					a.Credentials["access_token"] = "test-token"
					err = svc.testOpenAIImageOAuth(c, context.Background(), a, "gpt-image-2-codex", "cat")
				} else {
					err = svc.testOpenAIImageAPIKey(c, context.Background(), a, "gpt-image-2-codex", "cat")
				}
				require.Error(t, err)
				require.Equal(t, 1, repo.tempCalls)
				require.Empty(t, repo.modelRateLimitCalls)
				require.WithinDuration(t, time.Now().Add(10*time.Minute), repo.until, 2*time.Second)
				require.True(t, gateway.isOpenAIAccountRuntimeBlocked(a))
				var state TempUnschedState
				require.NoError(t, json.Unmarshal([]byte(repo.reason), &state))
				require.Equal(t, "no available image quota", state.MatchedKeyword)
				require.Len(t, recorder.entries, 1)
				require.Equal(t, 429, recorder.entries[0].StatusCode)
				require.Equal(t, "/api/v1/admin/accounts/222/test", recorder.entries[0].RequestPath)
				require.Equal(t, "req-quota-test", recorder.entries[0].UpstreamErrors[0].UpstreamRequestID)
			})
		}
	}
}
func fmtImageQuotaCase(oauth, wrapped bool) string {
	name := "apikey"
	if oauth {
		name = "oauth"
	}
	if wrapped {
		return name + "/wrapped-200"
	}
	return name + "/http-429"
}
func TestImageQuotaPolicy_ForwardImages(t *testing.T) {
	for _, scope := range []string{"account", "model", ""} {
		for _, pool := range []bool{false, true} {
			for _, mode := range []string{"http", "json", "sse"} {
				t.Run(scope+"/"+fmtImageQuotaCase(pool, false)+"/"+mode, func(t *testing.T) {
					repo := &imageQuotaPolicyRepo{}
					gateway := newOpenAIImagesTestService(nil)
					gateway.rateLimitService = &RateLimitService{accountRepo: repo, runtimeBlocker: gateway}
					a := imageQuotaPolicyAccount(scope, pool)
					body := `{"error":{"type":"rate_limit_error","message":"gpt-image-2: no available image quota"}}`
					status, contentType := http.StatusTooManyRequests, "application/json"
					if mode == "json" {
						status = http.StatusOK
					}
					if mode == "sse" {
						status, contentType = http.StatusOK, "text/event-stream"
						body = "data: " + body + "\n\n"
					}
					gateway.httpUpstream = &httpUpstreamRecorder{resp: &http.Response{StatusCode: status,
						Header: http.Header{"Content-Type": []string{contentType}, "X-Request-Id": []string{"req-quota"}},
						Body:   io.NopCloser(strings.NewReader(body))}}
					request := []byte(`{"model":"gpt-image-2","prompt":"cat"}`)
					c, _ := newOpenAIImagesTestContext(t, request)
					parsed, err := gateway.ParseOpenAIImagesRequest(c, request)
					require.NoError(t, err)
					parsed.Stream = mode == "sse"
					result, err := gateway.ForwardImages(context.Background(), c, a, request, parsed, "")
					require.Error(t, err)
					require.Nil(t, result)
					if scope == "account" {
						require.Equal(t, 1, repo.tempCalls)
						require.Empty(t, repo.modelRateLimitCalls)
						require.True(t, gateway.isOpenAIAccountRuntimeBlocked(a))
					} else {
						require.Zero(t, repo.tempCalls)
						require.Len(t, repo.modelRateLimitCalls, 1)
						require.Equal(t, "gpt-image-2", repo.modelRateLimitCalls[0].scope)
						require.WithinDuration(t, time.Now().Add(10*time.Minute), repo.modelRateLimitCalls[0].resetAt, 2*time.Second)
					}
					events, ok := c.Get(OpsUpstreamErrorsKey)
					require.True(t, ok)
					require.Len(t, events.([]*OpsUpstreamErrorEvent), 1)
					require.Equal(t, 429, events.([]*OpsUpstreamErrorEvent)[0].UpstreamStatusCode)
				})
			}
		}
	}
}
