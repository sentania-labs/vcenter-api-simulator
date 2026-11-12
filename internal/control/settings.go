package control

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

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
func (c *Controller) RuntimeSettings() RuntimeSettings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return RuntimeSettings{SessionTimeoutSeconds: int64(c.opts.SessionTimeout / time.Second)}
}
func (c *Controller) SetRuntimeSettings(s RuntimeSettings) (RuntimeSettings, error) {
	if s.SessionTimeoutSeconds < 0 || s.SessionTimeoutSeconds > int64((365*24*time.Hour)/time.Second) {
		return RuntimeSettings{}, invalid("session timeout must be between zero and one year")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := json.Marshal(s)
	if err != nil {
		return RuntimeSettings{}, err
	}
	if err = os.MkdirAll(c.opts.DataDir, 0700); err != nil {
		return RuntimeSettings{}, err
	}
	f, err := os.CreateTemp(c.opts.DataDir, ".runtime-settings-*")
	if err != nil {
		return RuntimeSettings{}, err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return RuntimeSettings{}, err
	}
	if err = f.Close(); err != nil {
		return RuntimeSettings{}, err
	}
	if err = os.Rename(f.Name(), c.settingsPath()); err != nil {
		return RuntimeSettings{}, err
	}
	c.opts.SessionTimeout = time.Duration(s.SessionTimeoutSeconds) * time.Second
	c.applied().sim.SetSessionTimeout(c.opts.SessionTimeout)
	return s, nil
}
