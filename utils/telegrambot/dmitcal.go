package telegrambot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	dmitCoronaUUID = "6c8a8e8d-74a9-4467-8129-2bad82dae684"
	dmitTinyUUID   = "0b46402c-62ab-4316-8b2d-22286b7f7bbf"
	noslaUUID      = "e447ea90-ae90-4580-b8e8-d223b5bdb83d"
)

var dmitCalRequestDir = "/app/data/dmit-wan-calibrator/reanchor-requests"

type dmitCalTarget struct {
	UUID       string
	Name       string
	LimitBytes int64
}

type dmitCalRequest struct {
	Version     int    `json:"version"`
	ClientUUID  string `json:"client_uuid"`
	UsedBytes   int64  `json:"used_bytes"`
	RequestedAt string `json:"requested_at"`
}

var dmitCalTargets = map[string]dmitCalTarget{
	"CORONA": {
		UUID:       dmitCoronaUUID,
		Name:       "DMIT LAX.AN4.EB.CORONA",
		LimitBytes: 2 * (1 << 40),
	},
	"TINY": {
		UUID:       dmitTinyUUID,
		Name:       "DMIT LAX.AS3.EB.TINY",
		LimitBytes: 1536 * (1 << 30),
	},
	"NOSLA": {
		UUID:       noslaUUID,
		Name:       "NoSLA 一周年纪念版-圣何塞",
		LimitBytes: 1000 * (1 << 30),
	},
}

func (b *bot) sendDMITCalibration(ctx context.Context, selector string) {
	target, usedBytes, err := parseDMITCalibrationCommand(selector)
	if err != nil {
		_ = b.send(ctx, "<b>流量校准</b>\n\n用法：\n<code>/dmitcal CORONA 1.07TB</code>\n<code>/dmitcal TINY 33.12GB</code>\n<code>/dmitcal NOSLA 83.52GB</code>\n<code>/noslacal 83.52GB</code>\n\n"+htmlEscape(err.Error()), nil)
		return
	}
	if err := writeDMITCalibrationRequest(target, usedBytes); err != nil {
		_ = b.send(ctx, "❌ 校准请求保存失败："+htmlEscape(err.Error()), nil)
		return
	}
	_ = b.send(ctx, fmt.Sprintf(
		"<b>✅ 流量校准请求已提交</b>\n\n节点：%s\n官方当前流量：%s\n套餐上限：%s\n\n系统将在数秒内更新；可稍后使用 /remaining %s 核对。",
		htmlEscape(target.Name),
		formatDMITBytes(usedBytes),
		formatDMITBytes(target.LimitBytes),
		strings.ToUpper(strings.Fields(selector)[0]),
	), nil)
}

func (b *bot) sendNoSLACalibration(ctx context.Context, selector string) {
	if strings.TrimSpace(selector) == "" {
		_ = b.send(ctx, "<b>NoSLA 流量校准</b>\n\n用法：<code>/noslacal 83.52GB</code>", nil)
		return
	}
	b.sendDMITCalibration(ctx, "NOSLA "+selector)
}

func parseDMITCalibrationCommand(selector string) (dmitCalTarget, int64, error) {
	fields := strings.Fields(strings.TrimSpace(selector))
	if len(fields) < 2 {
		return dmitCalTarget{}, 0, errors.New("请同时填写机器名和官方流量")
	}
	name := strings.ToUpper(fields[0])
	target, ok := dmitCalTargets[name]
	if !ok {
		return dmitCalTarget{}, 0, errors.New("只允许校准 CORONA、TINY 或 NOSLA")
	}
	usedBytes, err := parseDMITAmount(strings.Join(fields[1:], ""))
	if err != nil {
		return dmitCalTarget{}, 0, err
	}
	if usedBytes > target.LimitBytes {
		return dmitCalTarget{}, 0, fmt.Errorf("流量不能超过该机器套餐上限 %s", formatDMITBytes(target.LimitBytes))
	}
	return target, usedBytes, nil
}

func parseDMITAmount(raw string) (int64, error) {
	value := strings.ToUpper(strings.TrimSpace(raw))
	units := []struct {
		Suffix     string
		Multiplier float64
	}{
		{"TIB", 1 << 40}, {"TB", 1 << 40},
		{"GIB", 1 << 30}, {"GB", 1 << 30},
		{"MIB", 1 << 20}, {"MB", 1 << 20},
		{"KIB", 1 << 10}, {"KB", 1 << 10},
		{"B", 1},
	}
	for _, unit := range units {
		if !strings.HasSuffix(value, unit.Suffix) {
			continue
		}
		numberText := strings.TrimSpace(strings.TrimSuffix(value, unit.Suffix))
		number, err := strconv.ParseFloat(numberText, 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
			return 0, errors.New("流量格式无效，请使用例如 1.07TB 或 33.12GB")
		}
		bytesValue := number * unit.Multiplier
		if bytesValue > math.MaxInt64 {
			return 0, errors.New("流量数值过大")
		}
		return int64(math.Round(bytesValue)), nil
	}
	return 0, errors.New("必须填写单位 GB 或 TB")
}

func writeDMITCalibrationRequest(target dmitCalTarget, usedBytes int64) error {
	if target.UUID != dmitCoronaUUID && target.UUID != dmitTinyUUID && target.UUID != noslaUUID {
		return errors.New("目标不在流量校准白名单")
	}
	if err := os.MkdirAll(dmitCalRequestDir, 0700); err != nil {
		return err
	}
	request := dmitCalRequest{
		Version:     1,
		ClientUUID:  target.UUID,
		UsedBytes:   usedBytes,
		RequestedAt: time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dmitCalRequestDir, ".request-*.json")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(payload, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, filepath.Join(dmitCalRequestDir, target.UUID+".json"))
}

func formatDMITBytes(value int64) string {
	if value >= 1<<40 {
		return fmt.Sprintf("%.2f TB", float64(value)/(1<<40))
	}
	if value >= 1<<30 {
		return fmt.Sprintf("%.2f GB", float64(value)/(1<<30))
	}
	if value >= 1<<20 {
		return fmt.Sprintf("%.2f MB", float64(value)/(1<<20))
	}
	return fmt.Sprintf("%d B", value)
}

func htmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(value)
}
