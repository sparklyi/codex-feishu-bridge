package logs

import (
	"io"
	"os"
	"path/filepath"
	"time"
)

func DefaultDir(home string) string {
	return filepath.Join(home, ".codex-feishu-bridge", "logs")
}

func CreateRunLog(baseDir, taskID, runID string, content []byte) (string, error) {
	path, w, err := OpenRunLogWriter(baseDir, taskID, runID)
	if err != nil {
		return "", err
	}
	if _, err := w.Write(content); err != nil {
		_ = w.Close()
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	return path, nil
}

func OpenRunLogWriter(baseDir, taskID, runID string) (path string, w io.WriteCloser, err error) {
	taskDir := filepath.Join(baseDir, taskID)
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		return "", nil, err
	}
	_ = os.Chmod(baseDir, 0o700)
	_ = os.Chmod(taskDir, 0o700)
	path = filepath.Join(taskDir, runID+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", nil, err
	}
	_ = file.Chmod(0o600)
	return path, file, nil
}

func Prune(baseDir string, retentionDays int, now time.Time) error {
	if retentionDays <= 0 {
		return nil
	}
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	err := filepath.WalkDir(baseDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(cutoff) {
			return os.Remove(path)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
