package store

import (
	"database/sql"
	"errors"
	"time"
)

// Setting keys.
const (
	SettingAutoPower          = "auto_power"            // "1" = auto stop/start on
	SettingAutoPowerEnabledAt = "auto_power_enabled_at" // RFC3339, last time it was switched on
)

// Setting returns a stored setting, or "" if it was never set.
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`
		INSERT INTO app_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// AutoPower reports whether automatic stop/start is on, and since when.
func (s *Store) AutoPower() (on bool, since time.Time) {
	v, _ := s.Setting(SettingAutoPower)
	at, _ := s.Setting(SettingAutoPowerEnabledAt)
	return v == "1", parseTime(at)
}

// SetAutoPower switches automatic stop/start on or off. Switching it on
// records the time, which the scheduler uses as the earliest "idle since".
func (s *Store) SetAutoPower(on bool) error {
	if !on {
		return s.SetSetting(SettingAutoPower, "0")
	}
	if err := s.SetSetting(SettingAutoPowerEnabledAt, nowUTC()); err != nil {
		return err
	}
	return s.SetSetting(SettingAutoPower, "1")
}
