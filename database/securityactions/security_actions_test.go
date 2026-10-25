package securityactions

import (
	"testing"

	"github.com/komari-monitor/komari/internal/config"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestQueueLifecycle(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	config.SetDb(db)

	first, err := Enqueue("client-1", "203.0.113.9", ActionBan, 60)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	second, err := Enqueue("client-1", "203.0.113.9", ActionBan, 60)
	if err != nil {
		t.Fatalf("enqueue duplicate: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected duplicate pending action to be idempotent")
	}

	pending, err := PendingForClient("client-1")
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].Action != ActionBan {
		t.Fatalf("unexpected pending actions: %#v", pending)
	}

	acked, err := Ack("client-1", first.ID, StatusSuccess, "applied")
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if acked.Status != StatusSuccess || acked.Message != "applied" {
		t.Fatalf("unexpected acked action: %#v", acked)
	}

	pending, err = PendingForClient("client-1")
	if err != nil {
		t.Fatalf("pending after ack: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no pending actions after ack, got %#v", pending)
	}
}

func TestQueueRejectsUnsafeActionsBeforeStorage(t *testing.T) {
	cases := []struct {
		name, ip, action string
		duration         int
	}{
		{"shell", "203.0.113.1; id", ActionBan, 60},
		{"hostname", "example.com", ActionBan, 60},
		{"subnet", "203.0.113.0/24", ActionBan, 60},
		{"remote-exec", "203.0.113.1", "exec", 60},
		{"unbounded-duration", "203.0.113.1", ActionBan, 86401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Enqueue("node-1", tc.ip, tc.action, tc.duration); err == nil {
				t.Fatal("unsafe firewall action accepted")
			}
		})
	}
}

func TestQueueRejectsCrossClientAcknowledgement(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	config.SetDb(db)
	item, err := Enqueue("node-1", "2001:db8::1", ActionUnban, 60)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ack("node-2", item.ID, StatusSuccess, "spoofed"); err == nil {
		t.Fatal("another node acknowledged the action")
	}
	pending, err := PendingForClient("node-2")
	if err != nil || len(pending) != 0 {
		t.Fatalf("another node saw pending actions: %#v, %v", pending, err)
	}
	pending, err = PendingForClient("node-1")
	if err != nil || len(pending) != 1 || pending[0].Status != StatusPending {
		t.Fatalf("cross-node acknowledgement modified the action: %#v, %v", pending, err)
	}
	if _, err := Ack("node-1", item.ID, StatusSuccess, "applied"); err != nil {
		t.Fatal(err)
	}
}
