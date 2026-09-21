//go:build windows

package mpv

import (
	"io"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procPeekNamedPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

func ipcAddress(id string) (string, func(), error) {
	return `\\.\pipe\faro-mpv-` + id, func() {}, nil
}

// dialIPC opens mpv's named pipe with a synchronous, non-overlapped handle.
// Reads are gated on PeekNamedPipe so no blocking read is ever pending while
// commands are written: mpv's message-mode pipe server intermittently stops
// servicing clients that keep a blocking read pending across writes (commands
// are never read, and later writes eventually block or the pipe is closed).
func dialIPC(path string) (io.ReadWriteCloser, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(pointer,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		return nil, err
	}
	return &pollPipe{
		file: os.NewFile(uintptr(handle), path), handle: handle,
		writeWake: make(chan struct{}, 1),
	}, nil
}

type pollPipe struct {
	file      *os.File
	handle    windows.Handle
	writeWake chan struct{}
}

func (p *pollPipe) Read(buffer []byte) (int, error) {
	delay := 10 * time.Millisecond
	for {
		var available uint32
		result, _, callErr := procPeekNamedPipe.Call(
			uintptr(p.handle), 0, 0, 0,
			uintptr(unsafe.Pointer(&available)), 0)
		if result == 0 {
			return 0, callErr
		}
		if available > 0 {
			if uint32(len(buffer)) > available {
				buffer = buffer[:available]
			}
			// Data is already buffered in the pipe, so this returns immediately.
			return p.file.Read(buffer)
		}
		// A pending overlapped read triggers a command-loss bug in mpv and
		// mpv.net, so PeekNamedPipe is intentional. Back off while the player is
		// idle, and let writes wake the reader immediately for command replies.
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-p.writeWake:
			if !timer.Stop() {
				<-timer.C
			}
		}
		if delay < 100*time.Millisecond {
			delay *= 2
			if delay > 100*time.Millisecond {
				delay = 100 * time.Millisecond
			}
		}
	}
}

func (p *pollPipe) Write(buffer []byte) (int, error) {
	written, err := p.file.Write(buffer)
	if written > 0 {
		select {
		case p.writeWake <- struct{}{}:
		default:
		}
	}
	return written, err
}

func (p *pollPipe) Close() error { return p.file.Close() }

const jobObjectExtendedLimitInformation = 0x9

// assignProcessCleanup places the player process tree into a kill-on-close job
// object. The mpv launcher on Windows (mpv.com) spawns the real player as a
// child, so killing the launcher alone would orphan it. The kill-on-close flag
// also cleans up players if Faro exits without a graceful Close.
func assignProcessCleanup(process *os.Process) (func(), error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	var assignErr error
	if err := process.WithHandle(func(handle uintptr) {
		assignErr = windows.AssignProcessToJobObject(job, windows.Handle(handle))
	}); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	if assignErr != nil {
		_ = windows.CloseHandle(job)
		return nil, assignErr
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = windows.TerminateJobObject(job, 0)
			_ = windows.CloseHandle(job)
		})
	}, nil
}
