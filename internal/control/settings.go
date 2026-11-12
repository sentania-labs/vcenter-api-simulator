package control

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// RuntimeSettings are the settings the console saves beside the scenario.
type RuntimeSettings struct {
	SessionTimeoutSeconds int64 `json:"sessionTimeoutSeconds"`
}

func (c *Controller) settingsPath() string {
	return filepath.Join(c.opts.DataDir, "runtime-settings.json")
}

func (c *Controller) loadRuntimeSettings() error {
	b, err := os.ReadFile(c.settingsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var s RuntimeSettings
	if err = json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s.SessionTimeoutSeconds < 0 || s.SessionTimeoutSeconds > int64((365*24*time.Hour)/time.Second) {
		return errors.New("invalid saved session timeout")
	}
	c.opts.SessionTimeout = time.Duration(s.SessionTimeoutSeconds) * time.Second
	return nil
}
