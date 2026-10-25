package telegrambot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseDMITCalibrationCommand(t *testing.T) {
	target, used, err := parseDMITCalibrationCommand("CORONA 1.07 TB")
	require.NoError(t, err)
	require.Equal(t, dmitCoronaUUID, target.UUID)
	require.Equal(t, int64(1176477441720), used)

	target, used, err = parseDMITCalibrationCommand("tiny 33.12GB")
	require.NoError(t, err)
	require.Equal(t, dmitTinyUUID, target.UUID)
	require.Equal(t, int64(35562329211), used)

	target, used, err = parseDMITCalibrationCommand("nosla 83.52GB")
	require.NoError(t, err)
	require.Equal(t, noslaUUID, target.UUID)
	require.Equal(t, int64(89678917140), used)
}

func TestParseDMITCalibrationRejectsNonTargetAndOversize(t *testing.T) {
	_, _, err := parseDMITCalibrationCommand("OTHER 1GB")
	require.ErrorContains(t, err, "只允许")
	_, _, err = parseDMITCalibrationCommand("TINY 2TB")
	require.ErrorContains(t, err, "套餐上限")
}

func TestWriteDMITCalibrationRequest(t *testing.T) {
	oldDir := dmitCalRequestDir
	dmitCalRequestDir = t.TempDir()
	t.Cleanup(func() { dmitCalRequestDir = oldDir })

	target := dmitCalTargets["CORONA"]
	require.NoError(t, writeDMITCalibrationRequest(target, 12345))
	payload, err := os.ReadFile(filepath.Join(dmitCalRequestDir, dmitCoronaUUID+".json"))
	require.NoError(t, err)
	var request dmitCalRequest
	require.NoError(t, json.Unmarshal(payload, &request))
	require.Equal(t, dmitCoronaUUID, request.ClientUUID)
	require.Equal(t, int64(12345), request.UsedBytes)
}
