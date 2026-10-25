package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/komari-monitor/komari/internal/metricstore"
	logger "github.com/komari-monitor/komari/utils/log"
	"gorm.io/gorm"
	"math"
	"strings"
	"time"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/database/tasks"
	"github.com/komari-monitor/komari/utils"

	"github.com/google/uuid"
)

func DeleteClient(clientUuid string) error {
	db := dbcore.GetDBInstance()
	err := db.Delete(&models.Client{}, "uuid = ?", clientUuid).Error
	if err != nil {
		return err
	}
	return nil
}

func SaveClientInfo(update map[string]interface{}) error {
	db := dbcore.GetDBInstance()
	clientUUID, ok := update["uuid"].(string)
	if !ok || clientUUID == "" {
		return fmt.Errorf("invalid client UUID")
	}

	// 确保更新的字段不为空
	if len(update) == 0 {
		return fmt.Errorf("no fields to update")
	}

	update["updated_at"] = time.Now().UTC()

	toFloat64 := func(value interface{}) (float64, bool) {
		switch typed := value.(type) {
		case float64:
			return typed, true
		case float32:
			return float64(typed), true
		case int:
			return float64(typed), true
		case int8:
			return float64(typed), true
		case int16:
			return float64(typed), true
		case int32:
			return float64(typed), true
		case int64:
			return float64(typed), true
		case uint:
			return float64(typed), true
		case uint8:
			return float64(typed), true
		case uint16:
			return float64(typed), true
		case uint32:
			return float64(typed), true
		case uint64:
			return float64(typed), true
		case json.Number:
			parsed, err := typed.Float64()
			if err != nil {
				return 0, false
			}
			return parsed, true
		default:
			return 0, false
		}
	}

	checkOptionalInt := func(name, key string, maxValue float64) error {
		value, exists := update[key]
		if !exists || value == nil {
			return nil
		}

		numericValue, ok := toFloat64(value)
		if !ok {
			return fmt.Errorf("%s must be a valid number", name)
		}
		if math.IsNaN(numericValue) || math.IsInf(numericValue, 0) || math.Trunc(numericValue) != numericValue || numericValue < 0 || numericValue > maxValue || numericValue >= float64(math.MaxInt64) {
			return fmt.Errorf("%s must be a valid non-negative number: %v", name, value)
		}
		return nil
	}

	verify := func(update map[string]interface{}) error {
		if err := checkOptionalInt("Cpu.Cores", "cpu_cores", math.MaxInt-1); err != nil {
			return err
		}
		if err := checkOptionalInt("Cpu.PhysicalCores", "cpu_physical_cores", math.MaxInt-1); err != nil {
			return err
		}
		if err := checkOptionalInt("Ram.Total", "mem_total", math.MaxInt64-1); err != nil {
			return err
		}
		if err := checkOptionalInt("Swap.Total", "swap_total", math.MaxInt64-1); err != nil {
			return err
		}
		if err := checkOptionalInt("Disk.Total", "disk_total", math.MaxInt64-1); err != nil {
			return err
		}
		if err := checkOptionalInt("TrafficResetDay", "traffic_reset_day", 31); err != nil {
			return err
		}
		if err := checkOptionalInt("VnstatTotalUp", "vnstat_total_up", math.MaxInt64-1); err != nil {
			return err
		}
		if err := checkOptionalInt("VnstatTotalDown", "vnstat_total_down", math.MaxInt64-1); err != nil {
			return err
		}
		return nil
	}

	if err := verify(update); err != nil {
		return err
	}
	if err := normalizeVnstatBasicInfo(db, clientUUID, update); err != nil {
		return err
	}

	err := db.Model(&models.Client{}).Where("uuid = ?", clientUUID).Updates(update).Error
	if err != nil {
		return err
	}
	return nil
}

func normalizeVnstatBasicInfo(db *gorm.DB, clientUUID string, update map[string]interface{}) error {
	if days, ok := update["vnstat_days"]; ok {
		raw, err := json.Marshal(days)
		if err != nil {
			return fmt.Errorf("invalid vnStat daily data: %w", err)
		}
		if len(raw) > 256*1024 {
			return fmt.Errorf("vnStat daily data is too large")
		}
		update["vnstat_daily_json"] = string(raw)
		delete(update, "vnstat_days")
	}

	if iface, ok := update["vnstat_interface"].(string); ok {
		iface = strings.TrimSpace(iface)
		if len(iface) > 64 {
			return fmt.Errorf("vnStat interface name is too long")
		}
		update["vnstat_interface"] = iface
	}

	available, _ := update["vnstat_available"].(bool)
	if !available {
		return nil
	}

	var existing models.Client
	err := db.Select(
		"uuid",
		"vnstat_baseline_at",
	).Where("uuid = ?", clientUUID).First(&existing).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if !existing.VnstatBaselineAt.IsZero() {
		return nil
	}

	vnUp, okUp := int64FromUpdate(update["vnstat_total_up"])
	vnDown, okDown := int64FromUpdate(update["vnstat_total_down"])
	if !okUp || !okDown {
		return nil
	}
	latest, err := latestTrafficTotalsForBaseline(db, clientUUID)
	if err != nil {
		return err
	}
	update["vnstat_baseline_up"] = latest.Up
	update["vnstat_baseline_down"] = latest.Down
	update["vnstat_baseline_vn_up"] = vnUp
	update["vnstat_baseline_vn_down"] = vnDown
	update["vnstat_baseline_at"] = time.Now().UTC()
	return nil
}

func int64FromUpdate(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case float64:
		if typed < 0 || typed >= float64(math.MaxInt64) || math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed {
			return 0, false
		}
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

type trafficTotalsBaseline struct {
	Up   int64
	Down int64
}

func latestTrafficTotalsForBaseline(_ *gorm.DB, clientUUID string) (trafficTotalsBaseline, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	records, err := metricstore.GetLatestTrafficBefore(ctx, []string{clientUUID}, time.Now().UTC())
	if err != nil {
		return trafficTotalsBaseline{}, err
	}
	latest := records[clientUUID]
	return trafficTotalsBaseline{Up: latest.NetTotalUp, Down: latest.NetTotalDown}, nil
}

func EditClientName(clientUUID, clientName string) error {
	db := dbcore.GetDBInstance()
	err := db.Model(&models.Client{}).Where("uuid = ?", clientUUID).Update("name", clientName).Error
	if err != nil {
		return err
	}
	return nil
}

func EditClientToken(clientUUID, token string) error {
	db := dbcore.GetDBInstance()
	err := db.Model(&models.Client{}).Where("uuid = ?", clientUUID).Update("token", token).Error
	if err != nil {
		return err
	}
	return nil
}

// CreateClient 创建新客户端
func CreateClient() (clientUUID, token string, err error) {
	db := dbcore.GetDBInstance()
	token = utils.GenerateToken()
	clientUUID = uuid.New().String()

	client := models.Client{
		UUID:      clientUUID,
		Token:     token,
		Name:      "client_" + clientUUID[0:8],
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	err = db.Create(&client).Error
	if err != nil {
		return "", "", err
	}
	if err := tasks.AddDefaultOnClientUUID(clientUUID); err != nil {
		logger.ErrorArgs("clients", "Failed to apply default-on ping tasks to new client:", err)
	}
	return clientUUID, token, nil
}

func CreateClientWithName(name string) (clientUUID, token string, err error) {
	if name == "" {
		return CreateClient()
	}
	db := dbcore.GetDBInstance()
	token = utils.GenerateToken()
	clientUUID = uuid.New().String()
	client := models.Client{
		UUID:      clientUUID,
		Token:     token,
		Name:      name,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	err = db.Create(&client).Error
	if err != nil {
		return "", "", err
	}
	if err := tasks.AddDefaultOnClientUUID(clientUUID); err != nil {
		logger.ErrorArgs("clients", "Failed to apply default-on ping tasks to new client:", err)
	}
	return clientUUID, token, nil
}

/*
// GetAllClients 获取所有客户端配置

	func getAllClients() (clients []models.Client, err error) {
		db := dbcore.GetDBInstance()
		err = db.Find(&clients).Error
		if err != nil {
			return nil, err
		}
		return clients, nil
	}
*/
func GetClientByUUID(uuid string) (client models.Client, err error) {
	db := dbcore.GetDBInstance()
	err = db.Where("uuid = ?", uuid).First(&client).Error
	if err != nil {
		return models.Client{}, err
	}
	return client, nil
}

func GetClientTokenByUUID(uuid string) (token string, err error) {
	db := dbcore.GetDBInstance()
	var client models.Client
	err = db.Where("uuid = ?", uuid).First(&client).Error
	if err != nil {
		return "", err
	}
	return client.Token, nil
}

func GetAllClientBasicInfo() (clients []models.Client, err error) {
	db := dbcore.GetDBInstance()
	err = db.Find(&clients).Error
	if err != nil {
		return nil, err
	}
	return clients, nil
}

func SaveClient(updates map[string]interface{}) error {
	db := dbcore.GetDBInstance()
	clientUUID, ok := updates["uuid"].(string)
	if !ok || clientUUID == "" {
		return fmt.Errorf("invalid client UUID")
	}

	// 确保更新的字段不为空
	if len(updates) == 0 {
		return fmt.Errorf("no fields to update")
	}

	if v, exists := updates["traffic_limit"]; exists {
		if val, ok := v.(float64); ok {
			if val < 0 || val > math.MaxInt64-1 {
				return fmt.Errorf("traffic_limit must be a valid non-negative int64 value, got %v", val)
			}
		}
	}
	if value, exists := updates["expired_at"]; exists {
		switch typed := value.(type) {
		case nil:
			updates["expired_at"] = nil
		case time.Time:
			updates["expired_at"] = typed.UTC()
		case *time.Time:
			if typed == nil {
				updates["expired_at"] = nil
			} else {
				updates["expired_at"] = typed.UTC()
			}
		case string:
			stamp, err := time.Parse(time.RFC3339Nano, typed)
			if err != nil {
				return fmt.Errorf("expired_at must be an RFC3339 timestamp with a timezone: %w", err)
			}
			updates["expired_at"] = stamp.UTC()
		default:
			return fmt.Errorf("expired_at must be an RFC3339 timestamp with a timezone")
		}
	}

	updates["updated_at"] = time.Now().UTC()

	err := db.Model(&models.Client{}).Where("uuid = ?", clientUUID).Updates(updates).Error
	if err != nil {
		return err
	}
	return nil
}
