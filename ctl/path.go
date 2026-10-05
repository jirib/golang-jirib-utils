package ctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// SocketName is the socket filename within the application runtime directory.
const SocketName = "socket"

// ErrNoRuntimeDir indicates no suitable runtime directory could be determined.
var ErrNoRuntimeDir = errors.New("ctl: no runtime directory available")

// DefaultSocketPath returns the standard control socket path: <runtime dir>/<app>/socket.
func DefaultSocketPath(app string) (string, error) {
	if app == "" {
		return "", errors.New("ctl: app name must not be empty")
	}
	dir, err := RuntimeDir(app)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, app, SocketName), nil
}

// RuntimeDir resolves the parent directory for app's socket directory.
// Resolution order: $XDG_RUNTIME_DIR, /run/user/<uid>, and os.TempDir().
func RuntimeDir(app string) (string, error) {
	if app == "" {
		return "", errors.New("ctl: app name must not be empty")
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir, nil
	}
	if uid := os.Getuid(); uid >= 0 {
		candidate := filepath.Join("/run/user", strconv.Itoa(uid))
		if isDir(candidate) {
			return candidate, nil
		}
	}
	return os.TempDir(), nil
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// ensureDir ensures dir exists with 0700 permissions and is owned by the current user.
func ensureDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("ctl: creating control socket directory %s: %w", dir, err)
		}
		fi, statErr := os.Stat(dir)
		if statErr != nil {
			return fmt.Errorf("ctl: checking control socket directory %s: %w", dir, statErr)
		}
		if !fi.IsDir() {
			return fmt.Errorf("ctl: control socket path %s exists but is not a directory", dir)
		}
		if uid, ok := statOwner(fi); ok {
			if me := uint32(os.Getuid()); uid != me {
				return fmt.Errorf("ctl: control socket directory %s is owned by uid %d, not %d: "+
					"refusing to bind a control socket inside a directory another user created", dir, uid, me)
			}
		}
		return nil
	}
	// Explicit chmod ensures mode 0700 regardless of umask.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("ctl: setting mode on control socket directory %s: %w", dir, err)
	}
	return nil
}
