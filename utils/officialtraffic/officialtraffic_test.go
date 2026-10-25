package officialtraffic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseBandwagonSnapshot(t *testing.T) {
	body := []byte(`{
		"data_counter": 1920,
		"plan_monthly_data": 2000,
		"monthly_data_multiplier": 1048576,
		"data_next_reset": 1780000000
	}`)

	snapshot, err := parseBandwagonSnapshot("node-1", SourceConfig{Provider: "bandwagon"}, body)
	if err != nil {
		t.Fatalf("parseBandwagonSnapshot returned error: %v", err)
	}

	if snapshot.ClientUUID != "node-1" {
		t.Fatalf("unexpected uuid: %s", snapshot.ClientUUID)
	}
	if snapshot.UsedBytes != 1920*1048576 {
		t.Fatalf("unexpected used bytes: %d", snapshot.UsedBytes)
	}
	if snapshot.LimitBytes != 2000*1048576 {
		t.Fatalf("unexpected limit bytes: %d", snapshot.LimitBytes)
	}
	if snapshot.RemainingBytes != 80*1048576 {
		t.Fatalf("unexpected remaining bytes: %d", snapshot.RemainingBytes)
	}
	if snapshot.ResetAt.IsZero() || snapshot.ResetAt.Unix() != 1780000000 {
		t.Fatalf("unexpected reset time: %s", snapshot.ResetAt.Format(time.RFC3339))
	}
}

func TestParseBandwagonSnapshotCapsNegativeRemaining(t *testing.T) {
	body := []byte(`{
		"data_counter": 2200,
		"plan_monthly_data": 2000,
		"monthly_data_multiplier": 1048576
	}`)

	snapshot, err := parseBandwagonSnapshot("node-1", SourceConfig{Provider: "bandwagon"}, body)
	if err != nil {
		t.Fatalf("parseBandwagonSnapshot returned error: %v", err)
	}
	if snapshot.RemainingBytes != 0 {
		t.Fatalf("remaining should be capped at zero, got %d", snapshot.RemainingBytes)
	}
}

func TestParseBandwagonSnapshotProviderError(t *testing.T) {
	_, err := parseBandwagonSnapshot("node-1", SourceConfig{Provider: "bandwagon"}, []byte(`{"error":"invalid api key"}`))
	if err == nil {
		t.Fatal("expected provider error")
	}
}

func TestParseVPSHostingSnapshotBytes(t *testing.T) {
	body := []byte(`{
		"data": {
			"used_bytes": 1073741824,
			"limit_bytes": 1099511627776,
			"remaining_bytes": 1098437885952,
			"reset_at": "2026-08-09T00:00:00Z"
		}
	}`)

	snapshot, err := parseVPSHostingSnapshot("vps-1", SourceConfig{Provider: "vps-hosting"}, body)
	if err != nil {
		t.Fatalf("parseVPSHostingSnapshot returned error: %v", err)
	}
	if snapshot.UsedBytes != 1<<30 || snapshot.LimitBytes != 1<<40 || snapshot.RemainingBytes != 1098437885952 {
		t.Fatalf("unexpected traffic values: %#v", snapshot)
	}
	if snapshot.ResetAt.IsZero() || snapshot.ResetAt.Year() != 2026 {
		t.Fatalf("unexpected reset time: %s", snapshot.ResetAt.Format(time.RFC3339))
	}
}

func TestParseVPSHostingSnapshotHumanUnitsAndDerivedValues(t *testing.T) {
	body := []byte(`{
		"bandwidth": {
			"uploaded": "1.5GB",
			"downloaded": "512 MB",
			"quota": "10 GB"
		}
	}`)

	snapshot, err := parseVPSHostingSnapshot("vps-2", SourceConfig{Provider: "vps-hosting"}, body)
	if err != nil {
		t.Fatalf("parseVPSHostingSnapshot returned error: %v", err)
	}
	expectedUsed := int64(1.5*float64(1<<30) + 512*1024*1024)
	if snapshot.UsedBytes != expectedUsed {
		t.Fatalf("unexpected derived used bytes: %d", snapshot.UsedBytes)
	}
	if snapshot.LimitBytes != 10*(1<<30) || snapshot.RemainingBytes != snapshot.LimitBytes-snapshot.UsedBytes {
		t.Fatalf("unexpected derived remaining values: %#v", snapshot)
	}
}

func TestFetchVPSHostingSnapshotUsesBearerTokenAndServiceID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/service/42/bandwidth" {
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("unexpected authorization header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"used": 10, "limit": 100, "remaining": 90}`))
	}))
	defer server.Close()

	snapshot, err := fetchVPSHostingSnapshot(context.Background(), "vps-3", SourceConfig{
		Provider:  "vps-hosting",
		Endpoint:  server.URL + "/api",
		ServiceID: "42",
		Token:     "test-token",
	})
	if err != nil {
		t.Fatalf("fetchVPSHostingSnapshot returned error: %v", err)
	}
	if snapshot.UsedBytes != 10 || snapshot.LimitBytes != 100 || snapshot.RemainingBytes != 90 {
		t.Fatalf("unexpected snapshot: %#v", snapshot)
	}
}

func TestRedactSensitiveError(t *testing.T) {
	raw := `Get "https://api.64clouds.com/v1/getServiceInfo?api_key=private_secret&veid=123": context deadline exceeded`
	got := redactSensitive(raw)
	if got == raw {
		t.Fatal("expected redacted error")
	}
	if strings.Contains(got, "private_secret") {
		t.Fatalf("unexpected secret in output: %s", got)
	}
	if got != `Get "https://api.64clouds.com/v1/getServiceInfo?api_key=***&veid=123": context deadline exceeded` {
		t.Fatalf("unexpected redacted error: %s", got)
	}
}
