package sshlogin

import (
	"fmt"
	"net"
	"strings"

	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func normalizeWhitelist(values models.StringArray) (models.StringArray, error) {
	normalized := make(models.StringArray, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "\n") || strings.Contains(entry, "\r") {
			return nil, fmt.Errorf("IP whitelist entries must be single-line values")
		}
		if strings.Contains(entry, "/") {
			if _, _, err := net.ParseCIDR(entry); err != nil {
				return nil, fmt.Errorf("invalid IP/CIDR whitelist entry: %s", entry)
			}
		} else if net.ParseIP(entry) == nil {
			return nil, fmt.Errorf("invalid IP whitelist entry: %s", entry)
		}
		if _, ok := seen[entry]; ok {
			continue
		}
		seen[entry] = struct{}{}
		normalized = append(normalized, entry)
	}
	return normalized, nil
}

func ValidateNotifications(notifications []models.SSHLoginNotification) error {
	for i := range notifications {
		if strings.TrimSpace(notifications[i].Client) == "" {
			return fmt.Errorf("client UUID cannot be empty")
		}
		whitelist, err := normalizeWhitelist(notifications[i].IPWhitelist)
		if err != nil {
			return err
		}
		notifications[i].IPWhitelist = whitelist
	}
	return nil
}

func DefaultNotification(clientUUID string) models.SSHLoginNotification {
	return models.SSHLoginNotification{
		Client:      clientUUID,
		Enable:      true,
		IPWhitelist: models.StringArray{},
	}
}

func GetNotification(clientUUID string) (models.SSHLoginNotification, error) {
	db := dbcore.GetDBInstance()
	var notification models.SSHLoginNotification
	err := db.Where("client = ?", clientUUID).First(&notification).Error
	if err == nil {
		if notification.IPWhitelist == nil {
			notification.IPWhitelist = models.StringArray{}
		}
		return notification, nil
	}
	if err == gorm.ErrRecordNotFound {
		return DefaultNotification(clientUUID), nil
	}
	return notification, err
}

func ListNotifications() ([]models.SSHLoginNotification, error) {
	db := dbcore.GetDBInstance()
	var notifications []models.SSHLoginNotification
	err := db.Model(&models.SSHLoginNotification{}).Preload("ClientInfo").Find(&notifications).Error
	return notifications, err
}

func EditNotifications(notifications []models.SSHLoginNotification) error {
	if len(notifications) == 0 {
		return fmt.Errorf("at least one notification is required")
	}
	if err := ValidateNotifications(notifications); err != nil {
		return err
	}
	db := dbcore.GetDBInstance()
	return db.Model(&models.SSHLoginNotification{}).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "client"}},
			DoUpdates: clause.AssignmentColumns([]string{"enable", "ip_whitelist"}),
		}).
		Select("client", "enable", "ip_whitelist").
		Create(notifications).Error
}

func SetNotificationEnable(uuids []string, enable bool) error {
	if len(uuids) == 0 {
		return fmt.Errorf("at least one client UUID is required")
	}
	notifications := make([]models.SSHLoginNotification, 0, len(uuids))
	for _, uuid := range uuids {
		uuid = strings.TrimSpace(uuid)
		if uuid == "" {
			return fmt.Errorf("client UUID cannot be empty")
		}
		notifications = append(notifications, models.SSHLoginNotification{Client: uuid, Enable: enable})
	}
	db := dbcore.GetDBInstance()
	return db.Model(&models.SSHLoginNotification{}).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "client"}},
			DoUpdates: clause.AssignmentColumns([]string{"enable"}),
		}).
		Select("client", "enable").
		Create(notifications).Error
}

func CreateEvent(event models.SSHLoginEvent) error {
	return dbcore.GetDBInstance().Create(&event).Error
}

func ListEvents(clientUUID string, limit int) ([]models.SSHLoginEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	db := dbcore.GetDBInstance()
	var events []models.SSHLoginEvent
	query := db.Model(&models.SSHLoginEvent{}).Preload("ClientInfo").Order("occurred_at DESC").Limit(limit)
	if strings.TrimSpace(clientUUID) != "" {
		query = query.Where("client = ?", strings.TrimSpace(clientUUID))
	}
	err := query.Find(&events).Error
	return events, err
}

func DeleteEvents(clientUUID string) error {
	db := dbcore.GetDBInstance()
	query := db.Model(&models.SSHLoginEvent{})
	if strings.TrimSpace(clientUUID) != "" {
		query = query.Where("client = ?", strings.TrimSpace(clientUUID))
	} else {
		query = query.Session(&gorm.Session{AllowGlobalUpdate: true})
	}
	return query.Delete(&models.SSHLoginEvent{}).Error
}
