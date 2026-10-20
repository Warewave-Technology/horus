package sdnotify

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestNotify(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if ok, err := Notify(Ready); ok || err != nil {
		t.Fatalf("without socket: %v %v", ok, err)
	}

	path := filepath.Join(t.TempDir(), "notify.sock")
	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	t.Setenv("NOTIFY_SOCKET", path)
	if ok, err := Notify(Ready); !ok || err != nil {
		t.Fatalf("notify: %v %v", ok, err)
	}
	buf := make([]byte, 64)
	l.SetReadDeadline(time.Now().Add(time.Second))
	n, err := l.Read(buf)
	if err != nil || string(buf[:n]) != Ready {
		t.Fatalf("got %q %v", buf[:n], err)
	}
}

func TestWatchdogInterval(t *testing.T) {
	t.Setenv("WATCHDOG_PID", "")
	t.Setenv("WATCHDOG_USEC", "30000000")
	if iv := WatchdogInterval(); iv != 15*time.Second {
		t.Fatalf("got %v", iv)
	}
	t.Setenv("WATCHDOG_PID", "1")
	if iv := WatchdogInterval(); iv != 0 {
		t.Fatalf("other pid: got %v", iv)
	}
}
