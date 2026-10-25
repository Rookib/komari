package agent

import (
	"errors"
)

var (
	ErrSwitchVersionInvalidParams = errors.New("agent uuid and version are required")
	ErrSwitchVersionOffline       = errors.New("agent is offline")
	ErrSwitchVersionUnsupported   = errors.New("agent does not support v2 version switching")
)

func SwitchAgentVersion(uuid, version string) error {
	return ErrAgentControlDisabled
}
