// Package instance keeps a single Probe Desktop engine per user session.
//
// The first process takes an exclusive lock on a file of the data directory
// and publishes how to reach it. A second launch (double click on the app,
// login item firing twice) finds the lock taken, asks the running engine to
// show its window, and exits.
package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gvinsot/Probe/desktop/internal/config"
)

// ErrRunning reports that another engine already holds the lock.
var ErrRunning = errors.New("Probe Desktop is already running")

// Info tells a second launch how to reach the running engine.
type Info struct {
	PID     int    `json:"pid"`
	Port    int    `json:"port"`
	Control string `json:"control"`
}

// ControlHeader carries the control token on engine control requests.
const ControlHeader = "X-Probe-Control"

// Lock is held for the whole life of the engine.
type Lock struct {
	f   *os.File
	dir string
}

// Acquire takes the single-instance lock of a data directory.
func Acquire(dir string) (*Lock, error) {
	f, err := os.OpenFile(filepath.Join(dir, "instance.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, ErrRunning
	}
	return &Lock{f: f, dir: dir}, nil
}

// Publish records how to reach this engine. The file is readable by the
// user only: the control token lets its holder open the window.
func (l *Lock) Publish(info Info) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	path := filepath.Join(l.dir, "instance.json")
	if err := config.WriteFileAtomic(path, data); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Release drops the lock and the published information. Releasing twice is
// harmless: the engine releases early to hand over to its update.
func (l *Lock) Release() {
	if l.f == nil {
		return
	}
	os.Remove(filepath.Join(l.dir, "instance.json"))
	unlockFile(l.f)
	l.f.Close()
	l.f = nil
}

// Running returns the process that published its information, if any.
func Running(dir string) (Info, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "instance.json"))
	if err != nil {
		return Info{}, false
	}
	var info Info
	if json.Unmarshal(data, &info) != nil {
		return Info{}, false
	}
	return info, true
}

// Signal asks the running engine to perform a control action, such as
// "show" to open its window.
func Signal(dir, action string) error {
	info, ok := Running(dir)
	if !ok {
		return errors.New("no running engine published its address")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	url := fmt.Sprintf("http://127.0.0.1:%d/control/%s", info.Port, action)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set(ControlHeader, info.Control)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("engine answered %s", resp.Status)
	}
	return nil
}
