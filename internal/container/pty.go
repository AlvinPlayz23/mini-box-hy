//go:build linux

package container

import (
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// PTY is a pseudo-terminal pair.
type PTY struct {
	Master *os.File
	Slave  *os.File
}

// OpenPTY allocates a pseudo-terminal.
func OpenPTY() (*PTY, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, fmt.Errorf("unlock pty: %w", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("read pty number: %w", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("open pty slave /dev/pts/%d: %w", n, err)
	}
	return &PTY{Master: master, Slave: slave}, nil
}

// Close releases both ends.
func (p *PTY) Close() error {
	var err error
	if p.Slave != nil {
		err = p.Slave.Close()
	}
	if p.Master != nil {
		if merr := p.Master.Close(); err == nil {
			err = merr
		}
	}
	return err
}

// SetWinsize applies a terminal size to the pty.
func (p *PTY) SetWinsize(rows, cols uint16) error {
	ws := &unix.Winsize{Row: rows, Col: cols}
	if err := unix.IoctlSetWinsize(int(p.Master.Fd()), unix.TIOCSWINSZ, ws); err != nil {
		return fmt.Errorf("set pty window size: %w", err)
	}
	return nil
}

// TermState saves a terminal's settings so they can be restored.
type TermState struct {
	fd    int
	state *unix.Termios
}

// MakeRaw switches a terminal into raw mode, returning the previous state.
func MakeRaw(f *os.File) (*TermState, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, fmt.Errorf("read terminal settings: %w", err)
	}
	raw := *old
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, fmt.Errorf("switch terminal to raw mode: %w", err)
	}
	return &TermState{fd: fd, state: old}, nil
}

// Restore puts the terminal back into its saved state.
func (t *TermState) Restore() {
	if t == nil || t.state == nil {
		return
	}
	_ = unix.IoctlSetTermios(t.fd, unix.TCSETS, t.state)
	t.state = nil
}

// TerminalSize reports the size of a terminal, defaulting to 24x80.
func TerminalSize(f *os.File) (rows, cols uint16) {
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Row == 0 || ws.Col == 0 {
		return 24, 80
	}
	return ws.Row, ws.Col
}

// CopyPTY shuttles bytes between the pty master and the user's terminal until
// either side closes.
func CopyPTY(master *os.File, stdin, stdout *os.File) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(stdout, master)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(master, stdin)
	}()
	wg.Wait()
}
