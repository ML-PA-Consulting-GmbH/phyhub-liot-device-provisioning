package main

import (
	"context"
	"os"
	"syscall"

	"golang.org/x/term"
)

// watchForReset opens /dev/console in raw mode and watches for the operator
// pressing Alt+R. When detected, the returned channel receives a single
// signal. Subsequent presses are ignored; one signal per Observer
// lifecycle is all the consumer needs.
//
// Best-effort: if /dev/console can't be opened or termios can't be set
// raw, the returned channel never fires and the rest of the flow continues
// without keyboard support. The fallback is silent because the systemd
// unit's StandardInput=null makes this strictly an enhancement.
//
// Recognises two byte sequences for Alt+R:
//   - ESC (0x1B) followed by 'r' or 'R' (what xterm-like terminals send
//     for Meta/Alt + key).
//   - 0xF2 (what some terminals send when Alt sets the high bit of 'r').
//
// On ctx cancellation the goroutine restores the original termios state
// and closes the file. The channel is left un-fired.
func watchForReset(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, 1)

	f, err := os.OpenFile("/dev/console", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return ch
	}

	fd := int(f.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		_ = f.Close()
		return ch
	}

	// Closing the file from the ctx-watcher unblocks the reader's
	// blocking Read so the goroutine can clean up promptly.
	go func() {
		<-ctx.Done()
		_ = f.Close()
	}()

	go func() {
		defer func() {
			_ = term.Restore(fd, oldState)
			_ = f.Close()
		}()

		buf := make([]byte, 16)
		var seenESC bool
		for {
			n, rerr := f.Read(buf)
			if rerr != nil {
				return
			}
			for i := 0; i < n; i++ {
				b := buf[i]
				if seenESC {
					seenESC = false
					if b == 'r' || b == 'R' {
						notify(ch)
						return
					}
					continue
				}
				switch b {
				case 0x1B: // ESC; possible Alt prefix
					seenESC = true
				case 0xF2: // Alt+r when Alt sets the high bit
					notify(ch)
					return
				}
			}
		}
	}()

	return ch
}

// notify is a non-blocking send that drops if the channel is already
// armed (size 1, single consumer, fire-once semantics).
func notify(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
