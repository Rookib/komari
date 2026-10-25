package securityconsole

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/models"
	"github.com/stretchr/testify/assert"
)

func TestMain(m *testing.M) {
	tempDir, err := os.MkdirTemp("", "komari-securityconsole-test-*")
	if err != nil {
		panic(err)
	}
	flags.DatabaseFile = filepath.Join(tempDir, "komari.db")

	code := m.Run()
	_ = os.RemoveAll(tempDir)
	os.Exit(code)
}

func TestAttackFromOldAuditLog(t *testing.T) {
	entry := models.Log{
		ID:      1,
		Message: "SSH auth guard alert: client=腾讯云 上海四区, ip=51.91.64.198, user=root, count=5",
		Time:    time.Date(2026, 6, 24, 14, 7, 50, 0, time.UTC),
	}

	attack, ok := attackFromLog(entry, clientResolver{}, map[string]IPState{})

	assert.True(t, ok)
	assert.Equal(t, "腾讯云 上海四区", attack.Client)
	assert.Equal(t, "51.91.64.198", attack.SourceIP)
	assert.Equal(t, "root", attack.User)
	assert.Equal(t, 5, attack.FailedCount)
	assert.Equal(t, "unknown", attack.Method)
	assert.Equal(t, "medium", attack.Risk)
	assert.Equal(t, "active", attack.Status)
}

func TestAttackFromNewAuditLog(t *testing.T) {
	entry := models.Log{
		ID:      2,
		Message: "SSH auth guard alert: client=DMIT LAX, uuid=client-1, ip=203.0.113.9, user=root,ubuntu, method=password,pam, count=21",
		Time:    time.Date(2026, 6, 24, 14, 8, 1, 0, time.UTC),
	}

	attack, ok := attackFromLog(entry, clientResolver{}, map[string]IPState{
		stateKey("client-1", "203.0.113.9"): {
			Status: "banned",
			Client: "client-1",
			IP:     "203.0.113.9",
		},
	})

	assert.True(t, ok)
	assert.Equal(t, "client-1", attack.ClientUUID)
	assert.Equal(t, "root,ubuntu", attack.User)
	assert.Equal(t, "password,pam", attack.Method)
	assert.Equal(t, 21, attack.FailedCount)
	assert.Equal(t, "banned", attack.Status)
	assert.Equal(t, "high", attack.Risk)
}

func TestSuppressedEventFromLog(t *testing.T) {
	entry := models.Log{
		ID:      3,
		Message: "SSH auth guard alert suppressed: client=RackNerd uuid=client-2 ip=198.51.100.7 reason=cooldown",
		Time:    time.Date(2026, 6, 24, 14, 9, 0, 0, time.UTC),
	}

	event, ok := eventFromLog(entry, clientResolver{}, map[string]IPState{})

	assert.True(t, ok)
	assert.Equal(t, "SSHAuthGuardSuppressed", event.Type)
	assert.Equal(t, "RackNerd", event.Client)
	assert.Equal(t, "client-2", event.ClientUUID)
	assert.Equal(t, "198.51.100.7", event.SourceIP)
}

func TestValidIPOrCIDR(t *testing.T) {
	assert.True(t, validIPOrCIDR("203.0.113.1"))
	assert.True(t, validIPOrCIDR("2001:db8::1"))
	assert.True(t, validIPOrCIDR("203.0.113.0/24"))
	assert.False(t, validIPOrCIDR("203.0.113.1; rm -rf /"))
	assert.False(t, validIPOrCIDR("not-an-ip"))
}

func TestMatchMarkedStateCIDR(t *testing.T) {
	states := map[string]IPState{
		stateKey("client-1", "203.0.113.0/24"): {
			Status: "banned",
			Client: "client-1",
			IP:     "203.0.113.0/24",
		},
	}

	assert.Equal(t, "banned", matchMarkedState(states, "client-1", "203.0.113.9"))
	assert.Equal(t, "", matchMarkedState(states, "client-2", "203.0.113.9"))
	assert.Equal(t, "", matchMarkedState(states, "client-1", "198.51.100.9"))
}
