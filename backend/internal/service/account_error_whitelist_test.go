//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAccountErrorWhitelistIgnoresAutomaticUnschedulableState(t *testing.T) {
	now := time.Now()
	account := &Account{
		ID:                     901,
		Platform:               PlatformOpenAI,
		Type:                   AccountTypeOAuth,
		Status:                 StatusActive,
		Schedulable:            true,
		Extra:                  map[string]any{AccountErrorWhitelistExtraKey: true},
		RateLimitResetAt:       errorWhitelistTimePtr(now.Add(time.Hour)),
		OverloadUntil:          errorWhitelistTimePtr(now.Add(time.Hour)),
		TempUnschedulableUntil: errorWhitelistTimePtr(now.Add(time.Hour)),
	}

	require.True(t, account.IsErrorWhitelistEnabled())
	require.True(t, account.IsSchedulable())
	require.False(t, account.IsRateLimited())
	require.False(t, account.IsOverloaded())
}

func TestAccountErrorWhitelistStillHonorsManualAndNonErrorLimits(t *testing.T) {
	account := &Account{
		Status:      StatusActive,
		Schedulable: false,
		Extra:       map[string]any{AccountErrorWhitelistExtraKey: true},
	}
	require.False(t, account.IsSchedulable(), "manual schedulable=false must remain authoritative")

	account.Schedulable = true
	account.Status = StatusDisabled
	require.False(t, account.IsSchedulable(), "manual inactive status must remain authoritative")
}

func TestAccountErrorWhitelistSkipsErrorPolicyAndRuntimeBlock(t *testing.T) {
	repo := &errorPolicyRepoStub{}
	rateLimits := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	gateway := &OpenAIGatewayService{}
	rateLimits.SetAccountRuntimeBlocker(gateway)
	account := &Account{
		ID:          902,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Extra:       map[string]any{AccountErrorWhitelistExtraKey: true},
	}

	disabled := rateLimits.HandleUpstreamError(
		context.Background(), account, http.StatusUnauthorized, http.Header{}, []byte("invalid token"),
	)
	gateway.BlockAccountScheduling(account, time.Now().Add(time.Hour), "401")

	require.False(t, disabled)
	require.Zero(t, repo.setErrCalls)
	require.Zero(t, repo.tempCalls)
	require.False(t, gateway.isOpenAIAccountRuntimeBlocked(account))
}

func errorWhitelistTimePtr(value time.Time) *time.Time {
	return &value
}
