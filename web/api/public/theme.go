package public

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/internal/config"
	"github.com/komari-monitor/komari/web/api"
	"gorm.io/gorm"
)

type updateThemeModeRequest struct {
	ThemeMode string `json:"themeMode"`
}

func isAllowedPublicThemeMode(mode string) bool {
	return mode == "beijing" || mode == "light" || mode == "dark"
}

// UpdateThemeMode exposes a deliberately narrow public write endpoint for the
// active managed theme. Visitors may only update themeMode, so all devices can
// share the same display mode without granting admin access to full settings.
func UpdateThemeMode(c *gin.Context) {
	var req updateThemeModeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.RespondError(c, http.StatusBadRequest, "参数错误: "+err.Error())
		return
	}
	if !isAllowedPublicThemeMode(req.ThemeMode) {
		api.RespondError(c, http.StatusBadRequest, "主题模式只支持 beijing、light、dark")
		return
	}

	settings, err := config.GetManyAs[config.Settings]()
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "读取站点配置失败: "+err.Error())
		return
	}

	activeTheme := settings.Theme
	if activeTheme == "" || activeTheme == "default" {
		api.RespondError(c, http.StatusBadRequest, "当前主题不支持全站同步模式")
		return
	}

	db := dbcore.GetDBInstance()
	themeCfg := models.ThemeConfiguration{}
	err = db.Where("short = ?", activeTheme).First(&themeCfg).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		api.RespondError(c, http.StatusInternalServerError, "读取主题配置失败: "+err.Error())
		return
	}

	data := map[string]any{}
	if themeCfg.Data != "" {
		if err := json.Unmarshal([]byte(themeCfg.Data), &data); err != nil {
			api.RespondError(c, http.StatusBadRequest, "主题配置格式错误: "+err.Error())
			return
		}
	}
	data["themeMode"] = req.ThemeMode

	payload, err := json.Marshal(data)
	if err != nil {
		api.RespondError(c, http.StatusInternalServerError, "生成主题配置失败: "+err.Error())
		return
	}

	themeCfg.Short = activeTheme
	themeCfg.Data = string(payload)
	if err := db.Where("short = ?", activeTheme).
		Assign(models.ThemeConfiguration{Short: activeTheme, Data: themeCfg.Data}).
		FirstOrCreate(&themeCfg).Error; err != nil {
		api.RespondError(c, http.StatusInternalServerError, "保存主题配置失败: "+err.Error())
		return
	}

	api.RespondSuccess(c, gin.H{
		"theme":     activeTheme,
		"themeMode": req.ThemeMode,
	})
}
