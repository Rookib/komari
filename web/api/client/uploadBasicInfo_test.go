package client

import (
	"net"
	"testing"
	"time"

	"github.com/komari-monitor/komari/cmd/flags"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/config"
	v2 "github.com/komari-monitor/komari/protocol/v2"
	"github.com/komari-monitor/komari/utils/geoip"
)

type staticGeoIPProvider struct {
	name string
	iso  string
}

func (p staticGeoIPProvider) Name() string {
	return p.name
}

func (p staticGeoIPProvider) GetGeoInfo(ip net.IP) (*geoip.GeoInfo, error) {
	return &geoip.GeoInfo{ISOCode: p.iso, Name: p.iso}, nil
}

func (p staticGeoIPProvider) UpdateDatabase() error {
	return nil
}

func (p staticGeoIPProvider) Close() error {
	return nil
}

func TestV2BasicInfoFillsRegionFromGeoIP(t *testing.T) {
	flags.DatabaseType = "sqlite"
	flags.DatabaseFile = "file:v2_basic_info_geoip?mode=memory&cache=shared"

	db := dbcore.GetDBInstance()
	if err := config.Set(config.GeoIpEnabledKey, true); err != nil {
		t.Fatalf("enable geoip: %v", err)
	}

	oldProvider := geoip.CurrentProvider
	geoip.CurrentProvider = staticGeoIPProvider{name: t.Name(), iso: "SG"}
	t.Cleanup(func() {
		geoip.CurrentProvider = oldProvider
	})

	clientUUID := "client-v2-geoip"
	now := time.Now().UTC()
	if err := db.Create(&models.Client{
		UUID:      clientUUID,
		Token:     "token-v2-geoip",
		Name:      "client_v2_geoip",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("create client: %v", err)
	}

	resp := handleV2RPC(clientUUID, v2.Request{
		JSONRPC: v2.Version,
		Method:  v2.MethodAgentBasicInfo,
		Params: map[string]interface{}{
			"info": map[string]interface{}{
				"ipv4":                   "8.8.8.8",
				"traffic_reset_day":      15,
				"traffic_reset_timezone": "Asia/Shanghai",
			},
		},
		ID: "basic-info",
	}, false)
	if resp.Error != nil {
		t.Fatalf("v2 basic info failed: %+v", resp.Error)
	}

	var got models.Client
	if err := db.First(&got, "uuid = ?", clientUUID).Error; err != nil {
		t.Fatalf("load client: %v", err)
	}
	want := geoip.GetRegionUnicodeEmoji("SG")
	if got.Region != want {
		t.Fatalf("expected GeoIP region to be saved, got %q", got.Region)
	}
	if !got.TrafficResetReported || got.TrafficResetDay != 15 || got.TrafficResetTimezone != "Asia/Shanghai" {
		t.Fatalf("expected exact traffic reset config to be saved, got reported=%v day=%d timezone=%q", got.TrafficResetReported, got.TrafficResetDay, got.TrafficResetTimezone)
	}
}

func TestAgentBasicInfoCannotOverwriteAdministrativeFields(t *testing.T) {
	info := map[string]interface{}{"uuid": "other-node", "token": "replacement", "name": "renamed", "hidden": false,
		"price": 0, "traffic_limit": 0, "vnstat_baseline_up": 0, "cpu_name": "CPU", "traffic_reset_day": 15, "vnstat_total_up": 123}
	got := filterAgentBasicInfo(info)
	if len(got) != 3 || got["cpu_name"] != "CPU" || got["traffic_reset_day"] != 15 || got["vnstat_total_up"] != 123 {
		t.Fatalf("unexpected allowed telemetry fields: %v", got)
	}
}

func TestMalformedAgentIntegerMetadataDoesNotChangeStoredNode(t *testing.T) {
	db := dbcore.GetDBInstance()
	uuid := "client-basic-info-fraction"
	if err := db.Create(&models.Client{UUID: uuid, Token: "fraction-token", CpuCores: 4, TrafficResetDay: 15}).Error; err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]interface{}{"cpu_cores": 0.5, "traffic_reset_day": 15.5, "vnstat_total_up": float64(9223372036854775808.0)} {
		if err := saveClientBasicInfo(map[string]interface{}{key: value}, uuid, ""); err == nil {
			t.Fatalf("accepted malformed %s", key)
		}
	}
	var got models.Client
	if err := db.First(&got, "uuid = ?", uuid).Error; err != nil {
		t.Fatal(err)
	}
	if got.CpuCores != 4 || got.TrafficResetDay != 15 {
		t.Fatal("malformed metadata modified node")
	}
}
