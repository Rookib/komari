package agent

import (
	"context"
	"errors"
	v2 "github.com/komari-monitor/komari/protocol/v2"
	"testing"
)

func TestSafetyPolicyBlocksNewAgentControlAtEveryDeliveryBoundary(t *testing.T) {
	if _, err := GetStartupConfig(context.Background(), "node"); !errors.Is(err, ErrAgentControlDisabled) {
		t.Fatalf("startup config: %v", err)
	}
	if err := SwitchAgentVersion("node", "latest"); !errors.Is(err, ErrAgentControlDisabled) {
		t.Fatalf("switch version: %v", err)
	}
	for _, method := range []string{v2.MethodAgentStartupConfig, v2.MethodAgentSwitchVersion} {
		if DispatchV2Event("node", method, nil) {
			t.Fatal("dispatched disabled method")
		}
		if e := EnqueueV2Event("node", method, nil); e.ID != "" {
			t.Fatal("queued disabled method")
		}
	}
	if disabledAgentControl(v2.MethodAgentPing) {
		t.Fatal("TCP ping must remain allowed")
	}
}
