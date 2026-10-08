package wakewatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"shephrd/internal/model"
)

const ErrorWatchActive = "wake_watch_active"

type Holder struct {
	DriverID   string    `json:"driver_id"`
	Generation string    `json:"generation"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started_at"`
}

type Lock struct {
	file *os.File
}

func LockPath(dataDir, owner string) string {
	digest := sha256.Sum256([]byte(owner))
	return filepath.Join(dataDir, "watch", hex.EncodeToString(digest[:])+".lock")
}

func Acquire(dataDir string, holder Holder) (*Lock, error) {
	file, err := openLock(dataDir, holder.DriverID)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		var current Holder
		if errors.Is(syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB), syscall.EWOULDBLOCK) {
			current = readHolder(file)
		}
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, activeError(holder.DriverID, current, "another wake watch or an in-progress wake drain or pump holds the owner lock")
		}
		return nil, fmt.Errorf("lock watcher owner: %w", err)
	}
	body, err := json.Marshal(holder)
	if err == nil {
		if err = file.Truncate(0); err == nil {
			_, err = file.WriteAt(append(body, '\n'), 0)
		}
	}
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("record watcher lock holder: %w", err)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	return l.file.Close()
}

// Guard fences a manual drain or pump against an active watcher for the same
// owner. The shared lock is held until release so a watcher cannot start
// mid-command.
func Guard(dataDir, owner string) (func(), error) {
	file, err := openLock(dataDir, owner)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		current := readHolder(file)
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, activeError(owner, current, "an active wake watch owns notification consumption and activation for this owner")
		}
		return nil, fmt.Errorf("check watcher lock: %w", err)
	}
	return func() { file.Close() }, nil
}

func openLock(dataDir, owner string) (*os.File, error) {
	path := LockPath(dataDir, owner)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create watcher lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open watcher lock: %w", err)
	}
	return file, nil
}

func readHolder(file *os.File) Holder {
	var holder Holder
	body, err := io.ReadAll(io.LimitReader(io.NewSectionReader(file, 0, 4096), 4096))
	if err == nil {
		_ = json.Unmarshal(body, &holder)
	}
	return holder
}

func activeError(owner string, holder Holder, reason string) error {
	evidence := map[string]string{"driver_id": owner}
	if holder.Generation != "" {
		evidence["watcher_generation"] = holder.Generation
		evidence["watcher_pid"] = strconv.Itoa(holder.PID)
	}
	return model.EvidenceFailure(ErrorWatchActive, evidence, "%s for owner %s (watcher generation %q, pid %d); do not drain, pump, or start another watcher for this owner", reason, owner, holder.Generation, holder.PID)
}
