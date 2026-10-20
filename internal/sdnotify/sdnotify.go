// Package sdnotify implements the systemd notify protocol (sd_notify(3)) and
// watchdog pings without depending on libsystemd or go-systemd.
package sdnotify

import (
	"context"
	"net"
	"os"
	"strconv"
	"time"
)

const (
	Ready     = "READY=1"
	Stopping  = "STOPPING=1"
	Reloading = "RELOADING=1"
	Watchdog  = "WATCHDOG=1"
)

// Notify sends state to $NOTIFY_SOCKET. It returns (false, nil) when not
// running under systemd (no socket configured).
func Notify(state string) (bool, error) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return false, nil
	}
	if addr[0] == '@' { // abstract namespace
		addr = "\x00" + addr[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}

// Status sends a free-form STATUS= line shown by `systemctl status`.
func Status(s string) { _, _ = Notify("STATUS=" + s) }

// WatchdogInterval returns half of WatchdogSec when the watchdog is enabled
// for this process, or 0.
func WatchdogInterval() time.Duration {
	usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 0
	}
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0
	}
	return time.Duration(usec) * time.Microsecond / 2
}

// RunWatchdog pings the watchdog until ctx is done, as long as healthy()
// returns true. If healthy() fails, pings stop and systemd restarts the unit.
func RunWatchdog(ctx context.Context, healthy func() bool) {
	iv := WatchdogInterval()
	if iv == 0 {
		return
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if healthy == nil || healthy() {
				_, _ = Notify(Watchdog)
			}
		}
	}
}
