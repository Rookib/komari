package agent

import (
	"context"
	"errors"
	"sync"

	v2 "github.com/komari-monitor/komari/protocol/v2"
)

var (
	ErrAgentControlDisabled = errors.New("agent credential inspection and remote version switching are disabled by safety policy")
	ErrStartupConfigOffline = errors.New("agent is not connected")
	ErrStartupConfigTimeout = errors.New("agent startup configuration timed out; the agent may need an update")
	startupConfigMu         sync.Mutex
	startupConfigPending    = make(map[string]startupConfigCall)
)

type startupConfigCall struct {
	uuid     string
	response chan v2.StartupConfigResult
}

// GetStartupConfig holds credentials only for the lifetime of this request.
// The caller must require administrator access.
func GetStartupConfig(ctx context.Context, uuid string) (map[string]any, error) {
	return nil, ErrAgentControlDisabled
}

// ResolveStartupConfig binds the response to the authenticated agent, never to
// an agent ID supplied in the response body.
func ResolveStartupConfig(uuid string, result v2.StartupConfigResult) bool {
	startupConfigMu.Lock()
	defer startupConfigMu.Unlock()
	wait, ok := startupConfigPending[result.RequestID]
	if !ok || wait.uuid != uuid {
		return false
	}
	delete(startupConfigPending, result.RequestID)
	wait.response <- result
	return true
}
