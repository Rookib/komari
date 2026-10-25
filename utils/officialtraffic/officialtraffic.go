package officialtraffic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/config"
	"github.com/komari-monitor/komari/protocol/v2"
)

const (
	ConfigKey               = "official_traffic_sources"
	defaultBandwagonAPIURL  = "https://api.64clouds.com/v1/getServiceInfo"
	defaultVPSHostingAPIURL = "https://vps.hosting/api"
	defaultCacheTTL         = 5 * time.Minute
	requestTimeout          = 8 * time.Second
)

type SourceConfig struct {
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint,omitempty"`
	VEID     string `json:"veid,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	// VPS.hosting credentials. Token takes precedence over Username/Password.
	ServiceID       string `json:"service_id,omitempty"`
	Token           string `json:"token,omitempty"`
	Username        string `json:"username,omitempty"`
	Password        string `json:"password,omitempty"`
	TrafficUnit     string `json:"traffic_unit,omitempty"`
	DisplayName     string `json:"display_name,omitempty"`
	CacheTTLSeconds int    `json:"cache_ttl_seconds,omitempty"`

	// Collector/cache mode is used by a trusted local collector running on a
	// machine that has already passed a provider's browser challenge. It lets
	// the panel consume an official traffic snapshot without scraping from the
	// panel host.
	CollectorKey         string `json:"collector_key,omitempty"`
	CachedUsedBytes      int64  `json:"cached_used_bytes,omitempty"`
	CachedLimitBytes     int64  `json:"cached_limit_bytes,omitempty"`
	CachedRemainingBytes int64  `json:"cached_remaining_bytes,omitempty"`
	CachedResetAt        string `json:"cached_reset_at,omitempty"`
	CachedUpdatedAt      string `json:"cached_updated_at,omitempty"`
	NeedsVerification    bool   `json:"needs_verification,omitempty"`
	LastError            string `json:"last_error,omitempty"`
}

type Snapshot struct {
	ClientUUID     string
	Provider       string
	SourceName     string
	UsedBytes      int64
	LimitBytes     int64
	RemainingBytes int64
	ResetAt        time.Time
	UpdatedAt      time.Time
}

type CollectorUpdate struct {
	ClientUUID        string `json:"client_uuid"`
	CollectorKey      string `json:"collector_key,omitempty"`
	UsedBytes         int64  `json:"used_bytes,omitempty"`
	LimitBytes        int64  `json:"limit_bytes,omitempty"`
	RemainingBytes    int64  `json:"remaining_bytes,omitempty"`
	ResetAt           string `json:"reset_at,omitempty"`
	UpdatedAt         string `json:"updated_at,omitempty"`
	SourceName        string `json:"source_name,omitempty"`
	NeedsVerification bool   `json:"needs_verification,omitempty"`
	Error             string `json:"error,omitempty"`
}

type cachedSnapshot struct {
	snapshot      Snapshot
	ok            bool
	expiresAt     time.Time
	failureReason string
}

var (
	mu    sync.Mutex
	cache = map[string]cachedSnapshot{}
)

func ResetCacheForTest() {
	mu.Lock()
	defer mu.Unlock()
	cache = map[string]cachedSnapshot{}
}

func GetSnapshot(client models.Client) (*Snapshot, bool) {
	snapshot, ok := GetSnapshotForUUID(client.UUID)
	if !ok {
		return nil, false
	}
	if snapshot.LimitBytes <= 0 && client.TrafficLimit > 0 {
		snapshot.LimitBytes = client.TrafficLimit
		snapshot.RemainingBytes = maxInt64(snapshot.LimitBytes-snapshot.UsedBytes, 0)
	}
	return snapshot, true
}

func GetSnapshotForUUID(uuid string) (*Snapshot, bool) {
	cfg, ok := sourceConfigForUUID(uuid)
	if !ok || !cfg.Enabled {
		return nil, false
	}

	ttl := defaultCacheTTL
	if cfg.CacheTTLSeconds > 0 {
		ttl = time.Duration(cfg.CacheTTLSeconds) * time.Second
	}

	now := time.Now()
	mu.Lock()
	if item, exists := cache[uuid]; exists && now.Before(item.expiresAt) {
		if !item.ok {
			mu.Unlock()
			return nil, false
		}
		snapshot := item.snapshot
		mu.Unlock()
		return &snapshot, true
	}
	mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	snapshot, err := fetchSnapshot(ctx, uuid, cfg)
	if err != nil {
		reason := sanitizeError(err)
		slog.Warn("official traffic source fetch failed", "client", uuid, "provider", safeProvider(cfg.Provider), "error", reason)
		mu.Lock()
		cache[uuid] = cachedSnapshot{expiresAt: now.Add(ttl), failureReason: reason}
		mu.Unlock()
		return nil, false
	}

	mu.Lock()
	cache[uuid] = cachedSnapshot{snapshot: snapshot, ok: true, expiresAt: now.Add(ttl)}
	mu.Unlock()
	return &snapshot, true
}

func IsConfiguredForUUID(uuid string) bool {
	cfg, ok := sourceConfigForUUID(uuid)
	return ok && cfg.Enabled
}

func LastFailureReason(uuid string) (string, bool) {
	mu.Lock()
	item, ok := cache[uuid]
	mu.Unlock()
	if ok && !item.ok && strings.TrimSpace(item.failureReason) != "" {
		return item.failureReason, true
	}
	cfg, cfgOK := sourceConfigForUUID(uuid)
	if !cfgOK || strings.TrimSpace(cfg.LastError) == "" {
		return "", false
	}
	return cfg.LastError, true
}

func ApplyReportOverride(uuid string, report *v2.Report) bool {
	if report == nil {
		return false
	}
	snapshot, ok := GetSnapshotForUUID(uuid)
	if !ok {
		return false
	}
	report.Network.TotalUp = 0
	report.Network.TotalDown = snapshot.UsedBytes
	return true
}

func sourceConfigForUUID(uuid string) (cfg SourceConfig, ok bool) {
	defer func() {
		if recover() != nil {
			cfg = SourceConfig{}
			ok = false
		}
	}()
	sources, err := config.GetAs[map[string]SourceConfig](ConfigKey)
	if err != nil || len(sources) == 0 {
		return SourceConfig{}, false
	}
	if value, exists := sources[uuid]; exists {
		return value, true
	}
	for key, value := range sources {
		if strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(uuid)) {
			return value, true
		}
	}
	return SourceConfig{}, false
}

func fetchSnapshot(ctx context.Context, uuid string, cfg SourceConfig) (Snapshot, error) {
	switch normalizeProvider(cfg.Provider) {
	case "bandwagon", "bandwagonhost", "kiwivm", "64clouds":
		return fetchBandwagonSnapshot(ctx, uuid, cfg)
	case "vps", "vps-hosting", "vps_hosting", "vpshosting", "vps.hosting", "v.ps", "v.ps-hosting":
		return fetchVPSHostingSnapshot(ctx, uuid, cfg)
	case "collector-cache", "cache", "dmit-cache", "greencloud-cache", "manual-cache":
		return fetchCollectorCacheSnapshot(uuid, cfg)
	default:
		return Snapshot{}, fmt.Errorf("unsupported provider %q", safeProvider(cfg.Provider))
	}
}

func UpdateCollectorSnapshot(update CollectorUpdate, bearerToken string) (Snapshot, bool, error) {
	uuid := strings.TrimSpace(update.ClientUUID)
	if uuid == "" {
		return Snapshot{}, false, errors.New("missing client_uuid")
	}
	sources, err := config.GetAs[map[string]SourceConfig](ConfigKey)
	if err != nil || len(sources) == 0 {
		return Snapshot{}, false, errors.New("official traffic source is not configured")
	}
	cfg, key, ok := findSourceConfig(sources, uuid)
	if !ok || !cfg.Enabled {
		return Snapshot{}, false, errors.New("official traffic source is not enabled")
	}
	if !isCollectorCacheProvider(cfg.Provider) {
		return Snapshot{}, false, fmt.Errorf("provider %q does not accept collector cache updates", safeProvider(cfg.Provider))
	}
	expectedKey := strings.TrimSpace(cfg.CollectorKey)
	providedKey := strings.TrimSpace(update.CollectorKey)
	if providedKey == "" {
		providedKey = strings.TrimSpace(bearerToken)
	}
	if expectedKey == "" || providedKey == "" || providedKey != expectedKey {
		return Snapshot{}, false, errors.New("invalid collector key")
	}

	now := time.Now().UTC()
	cfg.LastError = sanitizeCollectorMessage(update.Error)
	cfg.NeedsVerification = update.NeedsVerification
	if strings.TrimSpace(update.SourceName) != "" {
		cfg.DisplayName = strings.TrimSpace(update.SourceName)
	}
	if update.NeedsVerification {
		if cfg.LastError == "" {
			cfg.LastError = "collector needs manual verification"
		}
		cfg.CachedUpdatedAt = now.Format(time.RFC3339)
		sources[key] = cfg
		if err := config.Set(ConfigKey, sources); err != nil {
			return Snapshot{}, false, err
		}
		invalidateCache(uuid)
		snapshot, hasSnapshot := snapshotFromCollectorConfig(uuid, cfg)
		return snapshot, hasSnapshot, nil
	}

	if update.UsedBytes < 0 || update.LimitBytes < 0 || update.RemainingBytes < 0 {
		return Snapshot{}, false, errors.New("traffic values must be non-negative")
	}
	if update.UsedBytes == 0 && update.LimitBytes == 0 {
		return Snapshot{}, false, errors.New("missing official traffic values")
	}
	cfg.CachedUsedBytes = update.UsedBytes
	cfg.CachedLimitBytes = update.LimitBytes
	if update.RemainingBytes > 0 {
		cfg.CachedRemainingBytes = update.RemainingBytes
	} else if update.LimitBytes > 0 {
		cfg.CachedRemainingBytes = maxInt64(update.LimitBytes-update.UsedBytes, 0)
	} else {
		cfg.CachedRemainingBytes = 0
	}
	cfg.CachedResetAt = normalizeTimeString(update.ResetAt)
	if normalized := normalizeTimeString(update.UpdatedAt); normalized != "" {
		cfg.CachedUpdatedAt = normalized
	} else {
		cfg.CachedUpdatedAt = now.Format(time.RFC3339)
	}
	cfg.NeedsVerification = false
	cfg.LastError = ""

	sources[key] = cfg
	if err := config.Set(ConfigKey, sources); err != nil {
		return Snapshot{}, false, err
	}
	invalidateCache(uuid)
	snapshot, ok := snapshotFromCollectorConfig(uuid, cfg)
	if !ok {
		return Snapshot{}, false, errors.New("saved collector snapshot is invalid")
	}
	return snapshot, true, nil
}

func fetchCollectorCacheSnapshot(uuid string, cfg SourceConfig) (Snapshot, error) {
	snapshot, ok := snapshotFromCollectorConfig(uuid, cfg)
	if ok {
		return snapshot, nil
	}
	if cfg.NeedsVerification {
		reason := strings.TrimSpace(cfg.LastError)
		if reason == "" {
			reason = "collector needs manual verification"
		}
		return Snapshot{}, errors.New(reason)
	}
	return Snapshot{}, errors.New("collector cache is empty")
}

func snapshotFromCollectorConfig(uuid string, cfg SourceConfig) (Snapshot, bool) {
	if cfg.CachedUsedBytes <= 0 && cfg.CachedLimitBytes <= 0 {
		return Snapshot{}, false
	}
	remaining := cfg.CachedRemainingBytes
	if remaining <= 0 && cfg.CachedLimitBytes > 0 {
		remaining = maxInt64(cfg.CachedLimitBytes-cfg.CachedUsedBytes, 0)
	}
	sourceName := strings.TrimSpace(cfg.DisplayName)
	if sourceName == "" {
		sourceName = "官方缓存"
	}
	resetAt := parseTimeString(cfg.CachedResetAt)
	updatedAt := parseTimeString(cfg.CachedUpdatedAt)
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	return Snapshot{
		ClientUUID:     uuid,
		Provider:       normalizeProvider(cfg.Provider),
		SourceName:     sourceName,
		UsedBytes:      cfg.CachedUsedBytes,
		LimitBytes:     cfg.CachedLimitBytes,
		RemainingBytes: remaining,
		ResetAt:        resetAt,
		UpdatedAt:      updatedAt,
	}, true
}

func findSourceConfig(sources map[string]SourceConfig, uuid string) (SourceConfig, string, bool) {
	if value, exists := sources[uuid]; exists {
		return value, uuid, true
	}
	for key, value := range sources {
		if strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(uuid)) {
			return value, key, true
		}
	}
	return SourceConfig{}, "", false
}

func isCollectorCacheProvider(provider string) bool {
	switch normalizeProvider(provider) {
	case "collector-cache", "cache", "dmit-cache", "greencloud-cache", "manual-cache":
		return true
	default:
		return false
	}
}

func invalidateCache(uuid string) {
	mu.Lock()
	defer mu.Unlock()
	delete(cache, uuid)
}

func fetchBandwagonSnapshot(ctx context.Context, uuid string, cfg SourceConfig) (Snapshot, error) {
	if strings.TrimSpace(cfg.VEID) == "" || strings.TrimSpace(cfg.APIKey) == "" {
		return Snapshot{}, errors.New("missing veid or api key")
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = defaultBandwagonAPIURL
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return Snapshot{}, fmt.Errorf("invalid endpoint: %w", err)
	}
	q := u.Query()
	q.Set("veid", strings.TrimSpace(cfg.VEID))
	q.Set("api_key", strings.TrimSpace(cfg.APIKey))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Snapshot{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Snapshot{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Snapshot{}, err
	}
	return parseBandwagonSnapshot(uuid, cfg, body)
}

func fetchVPSHostingSnapshot(ctx context.Context, uuid string, cfg SourceConfig) (Snapshot, error) {
	serviceID := strings.TrimSpace(cfg.ServiceID)
	if serviceID == "" {
		return Snapshot{}, errors.New("missing service_id")
	}

	endpoint := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if endpoint == "" {
		endpoint = defaultVPSHostingAPIURL
	}
	baseURL, err := url.Parse(endpoint)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return Snapshot{}, errors.New("invalid endpoint")
	}
	baseURL.Path = strings.TrimRight(baseURL.Path, "/") + "/service/" + url.PathEscape(serviceID) + "/bandwidth"
	baseURL.RawQuery = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL.String(), nil)
	if err != nil {
		return Snapshot{}, err
	}
	req.Header.Set("Accept", "application/json")
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		// APIKey is accepted as a backwards-compatible token alias.
		token = strings.TrimSpace(cfg.APIKey)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if strings.TrimSpace(cfg.Username) != "" && strings.TrimSpace(cfg.Password) != "" {
		req.SetBasicAuth(strings.TrimSpace(cfg.Username), cfg.Password)
	} else {
		return Snapshot{}, errors.New("missing token or username/password")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Snapshot{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Snapshot{}, err
	}
	return parseVPSHostingSnapshot(uuid, cfg, body)
}

func parseVPSHostingSnapshot(uuid string, cfg SourceConfig, body []byte) (Snapshot, error) {
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return Snapshot{}, err
	}
	if message := findStringValue(raw, "error", "error_message", "errorMessage", "detail"); strings.TrimSpace(message) != "" {
		return Snapshot{}, fmt.Errorf("provider error: %s", message)
	}

	unit := strings.TrimSpace(cfg.TrafficUnit)
	used, usedOK := findTrafficValue(raw, unit,
		"used_bytes", "usedBytes", "usage_bytes", "bandwidth_used", "bandwidth_usage", "data_used", "traffic_used", "used_traffic", "total_used", "data_counter", "consumed", "used")
	limit, limitOK := findTrafficValue(raw, unit,
		"limit_bytes", "limitBytes", "bandwidth_limit", "data_limit", "traffic_limit", "quota", "quota_bytes", "allocated", "plan_limit", "limit", "total")
	remaining, remainingOK := findTrafficValue(raw, unit,
		"remaining_bytes", "remainingBytes", "bandwidth_remaining", "data_remaining", "traffic_remaining", "available", "free_bytes", "free", "remaining")

	// Some providers expose only upload/download counters. Use them only as a
	// fallback; a field explicitly named "used" always wins.
	if !usedOK {
		var upload, download float64
		upload, uploadOK := findTrafficValue(raw, unit, "upload_bytes", "uploaded_bytes", "upload", "uploaded", "outbound", "egress")
		download, downloadOK := findTrafficValue(raw, unit, "download_bytes", "downloaded_bytes", "download", "downloaded", "inbound", "ingress")
		if uploadOK || downloadOK {
			used = upload + download
			usedOK = true
		}
	}
	if !usedOK && limitOK && remainingOK {
		used = limit - remaining
		usedOK = true
	}
	if !remainingOK && limitOK && usedOK {
		remaining = math.Max(limit-used, 0)
		remainingOK = true
	}
	if !usedOK && !limitOK && !remainingOK {
		return Snapshot{}, errors.New("missing traffic fields in VPS.hosting response")
	}

	usedBytes := safeInt64(used)
	limitBytes := safeInt64(limit)
	remainingBytes := safeInt64(remaining)
	if limitBytes > 0 && !remainingOK {
		remainingBytes = maxInt64(limitBytes-usedBytes, 0)
	}
	if limitBytes > 0 && remainingBytes > limitBytes {
		remainingBytes = limitBytes
	}
	if usedBytes > 0 && limitBytes > 0 && remainingBytes == 0 {
		remainingBytes = maxInt64(limitBytes-usedBytes, 0)
	}

	sourceName := strings.TrimSpace(cfg.DisplayName)
	if sourceName == "" {
		sourceName = "V.PS 官方 API"
	}
	provider := strings.TrimSpace(cfg.Provider)
	if provider == "" {
		provider = "vps-hosting"
	}
	return Snapshot{
		ClientUUID:     uuid,
		Provider:       normalizeProvider(provider),
		SourceName:     sourceName,
		UsedBytes:      usedBytes,
		LimitBytes:     limitBytes,
		RemainingBytes: remainingBytes,
		ResetAt:        parseVPSResetAt(raw),
		UpdatedAt:      time.Now(),
	}, nil
}

func findTrafficValue(raw any, unit string, keys ...string) (float64, bool) {
	for _, key := range keys {
		if value, found := findKeyValue(raw, key); found {
			if parsed, ok := parseTrafficValue(value, unit); ok {
				return parsed, true
			}
		}
	}
	return 0, false
}

func findStringValue(raw any, keys ...string) string {
	for _, key := range keys {
		if value, found := findKeyValue(raw, key); found {
			if text, ok := value.(string); ok {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func findKeyValue(raw any, wanted string) (any, bool) {
	wanted = normalizeJSONKey(wanted)
	switch value := raw.(type) {
	case map[string]any:
		for key, nested := range value {
			if normalizeJSONKey(key) == wanted {
				return nested, true
			}
		}
		for _, nested := range value {
			if found, ok := findKeyValue(nested, wanted); ok {
				return found, true
			}
		}
	case []any:
		for _, nested := range value {
			if found, ok := findKeyValue(nested, wanted); ok {
				return found, true
			}
		}
	}
	return nil, false
}

func normalizeJSONKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "_", "")
	key = strings.ReplaceAll(key, "-", "")
	key = strings.ReplaceAll(key, ".", "")
	return key
}

func parseTrafficValue(value any, defaultUnit string) (float64, bool) {
	if object, ok := value.(map[string]any); ok {
		unit := defaultUnit
		if valueUnit, exists := object["unit"]; exists {
			if text, ok := valueUnit.(string); ok && strings.TrimSpace(text) != "" {
				unit = text
			}
		}
		for _, key := range []string{"value", "amount", "bytes", "size", "total"} {
			if nested, exists := object[key]; exists {
				return parseTrafficValueWithUnit(nested, unit)
			}
		}
		return 0, false
	}
	return parseTrafficValueWithUnit(value, defaultUnit)
}

func parseTrafficValueWithUnit(value any, unit string) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number * trafficUnitMultiplier(unit), number >= 0
	case float32:
		return float64(number) * trafficUnitMultiplier(unit), number >= 0
	case int:
		return float64(number) * trafficUnitMultiplier(unit), number >= 0
	case int64:
		return float64(number) * trafficUnitMultiplier(unit), number >= 0
	case json.Number:
		parsed, err := number.Float64()
		return parsed * trafficUnitMultiplier(unit), err == nil && parsed >= 0
	case string:
		return parseTrafficString(number, unit)
	default:
		return 0, false
	}
}

func parseTrafficString(value, defaultUnit string) (float64, bool) {
	value = strings.TrimSpace(strings.ReplaceAll(value, ",", ""))
	if value == "" {
		return 0, false
	}
	fields := strings.Fields(value)
	if len(fields) == 1 {
		if parsed, err := strconv.ParseFloat(fields[0], 64); err == nil {
			return parsed * trafficUnitMultiplier(defaultUnit), parsed >= 0
		}
		compact := strings.ToUpper(fields[0])
		for _, suffix := range []string{"PIB", "PB", "TIB", "TB", "GIB", "GB", "MIB", "MB", "KIB", "KB", "B"} {
			if !strings.HasSuffix(compact, suffix) || len(compact) == len(suffix) {
				continue
			}
			parsed, err := strconv.ParseFloat(compact[:len(compact)-len(suffix)], 64)
			if err == nil && parsed >= 0 {
				return parsed * trafficUnitMultiplier(suffix), true
			}
		}
		return 0, false
	}
	parsed, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || parsed < 0 {
		return 0, false
	}
	return parsed * trafficUnitMultiplier(fields[1]), true
}

func trafficUnitMultiplier(unit string) float64 {
	switch strings.ToUpper(strings.TrimSpace(unit)) {
	case "B", "BYTE", "BYTES", "":
		return 1
	case "KB":
		return 1 << 10
	case "KIB":
		return 1 << 10
	case "MB":
		return 1 << 20
	case "MIB":
		return 1 << 20
	case "GB":
		return 1 << 30
	case "GIB":
		return 1 << 30
	case "TB":
		return 1 << 40
	case "TIB":
		return 1 << 40
	case "PB":
		return 1 << 50
	case "PIB":
		return 1 << 50
	default:
		return 1
	}
}

func parseVPSResetAt(raw any) time.Time {
	for _, key := range []string{"reset_at", "resetAt", "next_reset", "nextReset", "data_next_reset", "reset"} {
		if value, found := findKeyValue(raw, key); found {
			if parsed, ok := parseTimeValue(value); ok {
				return parsed
			}
		}
	}
	return time.Time{}
}

func parseTimeValue(value any) (time.Time, bool) {
	if number, ok := parseTrafficValue(value, ""); ok && number > 0 {
		seconds := number
		if seconds > 1e12 {
			seconds /= 1000
		}
		return time.Unix(safeInt64(seconds), 0), true
	}
	text, ok := value.(string)
	if !ok {
		return time.Time{}, false
	}
	text = strings.TrimSpace(text)
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func parseBandwagonSnapshot(uuid string, cfg SourceConfig, body []byte) (Snapshot, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return Snapshot{}, err
	}
	if msg := firstString(raw, "error", "message"); strings.TrimSpace(msg) != "" {
		return Snapshot{}, fmt.Errorf("provider error: %s", msg)
	}

	used, ok := numeric(raw, "data_counter", "dataCounter")
	if !ok {
		return Snapshot{}, errors.New("missing data_counter")
	}
	limit, _ := numeric(raw, "plan_monthly_data", "planMonthlyData")
	multiplier, ok := numeric(raw, "monthly_data_multiplier", "monthlyDataMultiplier")
	if !ok || multiplier <= 0 {
		multiplier = 1
	}

	usedBytes := safeInt64(used * multiplier)
	limitBytes := safeInt64(limit * multiplier)
	remainingBytes := int64(0)
	if limitBytes > 0 {
		remainingBytes = maxInt64(limitBytes-usedBytes, 0)
	}
	resetAt := parseResetAt(raw)
	sourceName := strings.TrimSpace(cfg.DisplayName)
	if sourceName == "" {
		sourceName = "BandwagonHost 官方 API"
	}
	provider := strings.TrimSpace(cfg.Provider)
	if provider == "" {
		provider = "bandwagon"
	}

	return Snapshot{
		ClientUUID:     uuid,
		Provider:       normalizeProvider(provider),
		SourceName:     sourceName,
		UsedBytes:      usedBytes,
		LimitBytes:     limitBytes,
		RemainingBytes: remainingBytes,
		ResetAt:        resetAt,
		UpdatedAt:      time.Now(),
	}, nil
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

func safeProvider(provider string) string {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return "unknown"
	}
	return normalizeProvider(provider)
}

func firstString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := raw[key]; ok {
			if s, ok := value.(string); ok {
				return s
			}
		}
	}
	return ""
}

func numeric(raw map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		switch v := value.(type) {
		case float64:
			return v, true
		case float32:
			return float64(v), true
		case int:
			return float64(v), true
		case int64:
			return float64(v), true
		case json.Number:
			f, err := v.Float64()
			return f, err == nil
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			return f, err == nil
		}
	}
	return 0, false
}

func parseResetAt(raw map[string]any) time.Time {
	if n, ok := numeric(raw, "data_next_reset", "dataNextReset"); ok && n > 0 {
		return time.Unix(safeInt64(n), 0)
	}
	for _, key := range []string{"next_reset", "reset_at", "data_reset_at"} {
		if s := firstString(raw, key); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

func safeInt64(f float64) int64 {
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
		return 0
	}
	if f > float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(math.Round(f))
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	return redactSensitive(err.Error())
}

func redactSensitive(s string) string {
	for _, key := range []string{"api_key", "token", "password", "collector_key"} {
		s = redactQueryParam(s, key)
	}
	return s
}

func redactQueryParam(s, key string) string {
	for _, sep := range []string{"?", "&"} {
		needle := sep + key + "="
		start := strings.Index(strings.ToLower(s), strings.ToLower(needle))
		if start < 0 {
			continue
		}
		valueStart := start + len(needle)
		valueEnd := len(s)
		if next := strings.IndexByte(s[valueStart:], '&'); next >= 0 {
			valueEnd = valueStart + next
		}
		s = s[:valueStart] + "***" + s[valueEnd:]
	}
	return s
}

func normalizeTimeString(value string) string {
	t := parseTimeString(value)
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTimeString(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil && unix > 0 {
		return time.Unix(unix, 0)
	}
	return time.Time{}
}

func sanitizeCollectorMessage(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = redactSensitive(value)
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	if len(value) > 240 {
		value = value[:240]
	}
	return value
}
