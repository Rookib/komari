package securityactions

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/komari-monitor/komari/internal/config"
)

const queueKey = "security_action_queue"

const (
	StatusPending = "pending"
	StatusSuccess = "success"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
)

const (
	ActionBan   = "ban"
	ActionUnban = "unban"
)

var queueMu sync.Mutex

type Action struct {
	ID        string `json:"id"`
	Client    string `json:"client"`
	IP        string `json:"ip"`
	Action    string `json:"action"`
	Duration  int    `json:"duration"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func Enqueue(clientUUID, ip, action string, duration int) (Action, error) {
	clientUUID = strings.TrimSpace(clientUUID)
	ip = strings.TrimSpace(ip)
	action = strings.TrimSpace(strings.ToLower(action))
	if clientUUID == "" {
		return Action{}, fmt.Errorf("client is required")
	}
	if net.ParseIP(ip) == nil {
		return Action{}, fmt.Errorf("a single valid IP is required")
	}
	if action != ActionBan && action != ActionUnban {
		return Action{}, fmt.Errorf("invalid action")
	}
	if duration <= 0 {
		duration = 3600
	}
	if duration > 86400 {
		return Action{}, fmt.Errorf("duration exceeds 86400 seconds")
	}

	queueMu.Lock()
	defer queueMu.Unlock()

	actions, err := loadLocked()
	if err != nil {
		return Action{}, err
	}
	for _, existing := range actions {
		if existing.Status == StatusPending &&
			existing.Client == clientUUID &&
			existing.IP == ip &&
			existing.Action == action {
			return existing, nil
		}
	}

	now := time.Now().Format(time.RFC3339)
	item := Action{
		ID:        uuid.NewString(),
		Client:    clientUUID,
		IP:        ip,
		Action:    action,
		Duration:  duration,
		Status:    StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	actions = append(actions, item)
	if err := saveLocked(prune(actions)); err != nil {
		return Action{}, err
	}
	return item, nil
}

func PendingForClient(clientUUID string) ([]Action, error) {
	clientUUID = strings.TrimSpace(clientUUID)
	queueMu.Lock()
	defer queueMu.Unlock()

	actions, err := loadLocked()
	if err != nil {
		return nil, err
	}
	pending := make([]Action, 0)
	for _, action := range actions {
		if action.Client == clientUUID && action.Status == StatusPending {
			pending = append(pending, action)
		}
	}
	return pending, nil
}

func Ack(clientUUID, id, status, message string) (Action, error) {
	clientUUID = strings.TrimSpace(clientUUID)
	id = strings.TrimSpace(id)
	status = strings.TrimSpace(strings.ToLower(status))
	if status != StatusSuccess && status != StatusFailed && status != StatusSkipped {
		return Action{}, fmt.Errorf("invalid status")
	}

	queueMu.Lock()
	defer queueMu.Unlock()

	actions, err := loadLocked()
	if err != nil {
		return Action{}, err
	}
	for i := range actions {
		if actions[i].ID != id || actions[i].Client != clientUUID {
			continue
		}
		actions[i].Status = status
		actions[i].Message = trimMessage(message, 240)
		actions[i].UpdatedAt = time.Now().Format(time.RFC3339)
		if err := saveLocked(prune(actions)); err != nil {
			return Action{}, err
		}
		return actions[i], nil
	}
	return Action{}, fmt.Errorf("action not found")
}

func loadLocked() ([]Action, error) {
	actions, err := config.GetAs[[]Action](queueKey, []Action{})
	if err != nil || actions == nil {
		return []Action{}, err
	}
	return actions, nil
}

func saveLocked(actions []Action) error {
	return config.Set(queueKey, actions)
}

func prune(actions []Action) []Action {
	const keepCompleted = 500
	pending := make([]Action, 0, len(actions))
	completed := make([]Action, 0, len(actions))
	for _, action := range actions {
		if action.Status == StatusPending {
			pending = append(pending, action)
		} else {
			completed = append(completed, action)
		}
	}
	if len(completed) > keepCompleted {
		completed = completed[len(completed)-keepCompleted:]
	}
	return append(completed, pending...)
}

func trimMessage(value string, maxLen int) string {
	value = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(value, "\n", " "), "\r", " "))
	if len(value) > maxLen {
		return value[:maxLen]
	}
	return value
}
