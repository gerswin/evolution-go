package msgstore

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gomessguii/logger"
)

const (
	envEnabled       = "MESSAGE_STORE_ENABLED"
	envPath          = "MESSAGE_STORE_PATH"
	envRetentionDays = "MESSAGE_STORE_RETENTION_DAYS"
)

// InitFromEnv opens the message store and installs it as the process default
// when MESSAGE_STORE_ENABLED=true. It returns nil (and no error) when the
// feature is off.
//
//	MESSAGE_STORE_PATH            SQLite file (default: <binary dir>/dbdata/messages.db)
//	MESSAGE_STORE_RETENTION_DAYS  delete messages older than N days (default 0 = keep forever)
func InitFromEnv() (*Store, error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(envEnabled)), "true") {
		return nil, nil
	}

	path := strings.TrimSpace(os.Getenv(envPath))
	if path == "" {
		dir := "."
		if exe, err := os.Executable(); err == nil {
			dir = filepath.Dir(exe)
		}
		path = filepath.Join(dir, "dbdata", "messages.db")
	}

	s, err := Open(path)
	if err != nil {
		return nil, err
	}
	SetDefault(s)
	logger.LogInfo("Message store enabled (SQLite): %s", path)

	if days, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(envRetentionDays))); days > 0 {
		go s.pruneLoop(time.Duration(days) * 24 * time.Hour)
	}

	return s, nil
}

func (s *Store) pruneLoop(retention time.Duration) {
	for {
		if n, err := s.Prune(time.Now().Add(-retention)); err != nil {
			logger.LogError("message store: prune failed: %v", err)
		} else if n > 0 {
			logger.LogInfo("message store: pruned %d messages older than %s", n, retention)
		}
		time.Sleep(6 * time.Hour)
	}
}
