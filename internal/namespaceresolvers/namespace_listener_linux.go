//go:build linux

package namespaceresolvers

import (
	"fmt"
	"net"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

type resolverListeners struct {
	udp net.PacketConn
	tcp net.Listener
}

func listenInNamespace(namespace uintptr) (resolverListeners, error) {
	type outcome struct {
		listeners resolverListeners
		err       error
	}
	result := make(chan outcome, 1)
	go func() {
		runtime.LockOSThread()
		host, err := os.Open("/proc/self/ns/net")
		if err != nil {
			runtime.UnlockOSThread()
			result <- outcome{err: err}
			return
		}
		defer host.Close()
		if err := unix.Setns(int(namespace), unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			result <- outcome{err: err}
			return
		}
		var listeners resolverListeners
		listeners.udp, err = net.ListenPacket("udp4", loopbackResolver+":53")
		if err == nil {
			listeners.tcp, err = net.Listen("tcp4", loopbackResolver+":53")
		}
		if err != nil && listeners.udp != nil {
			_ = listeners.udp.Close()
			listeners.udp = nil
		}
		restoreErr := unix.Setns(int(host.Fd()), unix.CLONE_NEWNET)
		if restoreErr != nil {
			closeResolverListeners(listeners)
			// Exiting while still locked terminates this OS thread instead of
			// returning a foreign-namespace thread to the Go scheduler.
			result <- outcome{err: fmt.Errorf("restore host network namespace: %w", restoreErr)}
			return
		}
		runtime.UnlockOSThread()
		result <- outcome{listeners: listeners, err: err}
	}()
	value := <-result
	return value.listeners, value.err
}

func closeResolverListeners(listeners resolverListeners) {
	if listeners.udp != nil {
		_ = listeners.udp.Close()
	}
	if listeners.tcp != nil {
		_ = listeners.tcp.Close()
	}
}
