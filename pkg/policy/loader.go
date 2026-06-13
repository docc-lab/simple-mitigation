package policy

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch blocks until ctx is cancelled, calling onReload(doc) every time
// the file at path changes. ConfigMap subPath mounts use a symlink swap
// (kubelet renames the parent ..data symlink rather than truncating the
// file), so the watcher subscribes to the *containing directory* and
// reacts to any non-CHMOD event -- a direct file-watch would miss the
// rename.
//
// onReload errors are logged but never propagated out of Watch; the caller
// has already committed to running until ctx is cancelled, and a transient
// typo in a hot ConfigMap shouldn't crash the controller.
//
// A 250ms debounce coalesces the WRITE+CREATE+RENAME+REMOVE storm that
// kubelet emits on every ConfigMap update into a single onReload call.
func Watch(ctx context.Context, path string, logger *slog.Logger, onReload func(*Document)) error {
	if logger == nil {
		logger = slog.Default()
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	dir := filepath.Dir(path)
	if err := watcher.Add(dir); err != nil {
		return err
	}

	const debounce = 250 * time.Millisecond
	var timer *time.Timer
	fire := func() {
		doc, err := LoadFile(path)
		if err != nil {
			logger.Warn("policy: reload failed; keeping previous rules",
				"path", path, "err", err)
			return
		}
		onReload(doc)
	}
	schedule := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(debounce, fire)
	}

	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			// ConfigMap (subPath) updates manifest as: new ..2025_xx dir + files,
			// rename of ..data symlink, removal of old ..xx dir. We can't reliably
			// pin the event to a single name -- accept any write/create/rename in
			// the parent dir and let the debouncer coalesce the burst. CHMOD by
			// itself is ignored so a `chmod` doesn't cause a noisy reload.
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			logger.Debug("policy: file event", "name", ev.Name, "op", ev.Op.String())
			schedule()
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			logger.Warn("policy: fsnotify error", "err", err)
		}
	}
}
