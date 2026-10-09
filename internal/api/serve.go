package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// Serve listens on a unix socket (and optionally a TCP address) until ctx is
// cancelled, serving NewHandler(b) with requests logged to stderr. It removes
// a stale socket file first, and removes it again on shutdown; the socket
// gets mode 0660.
func Serve(ctx context.Context, socketPath, tcpAddr string, b Backend) error {
	if socketPath == "" && tcpAddr == "" {
		return errors.New("nothing to listen on: socketPath and tcpAddr are both empty")
	}
	srv := &http.Server{Handler: withLogging(os.Stderr, NewHandler(b))}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	serve := func(ln net.Listener) {
		defer wg.Done()
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}
	}
	if socketPath != "" {
		if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", socketPath, err)
		}
		ln, err := net.Listen("unix", socketPath)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", socketPath, err)
		}
		if err := os.Chmod(socketPath, 0o660); err != nil {
			ln.Close()
			return fmt.Errorf("chmod %s: %w", socketPath, err)
		}
		defer removeSocketFile(socketPath)
		wg.Add(1)
		go serve(ln)
	}
	if tcpAddr != "" {
		ln, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			srv.Close()
			wg.Wait()
			return fmt.Errorf("listen on %s: %w", tcpAddr, err)
		}
		wg.Add(1)
		go serve(ln)
	}
	stopped := make(chan struct{})
	go func() { wg.Wait(); close(stopped) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			srv.Close()
		}
		<-stopped
	case <-stopped:
	}
	mu.Lock()
	defer mu.Unlock()
	return firstErr
}

// removeSocketFile deletes a leftover unix socket but never a regular file.
func removeSocketFile(path string) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		os.Remove(path)
	}
}

// withLogging wraps next with a middleware that logs method, path, status
// and duration to w.
func withLogging(w io.Writer, next http.Handler) http.Handler {
	logger := log.New(w, "", log.LstdFlags)
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: rw, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		logger.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.code, time.Since(start).Round(time.Microsecond))
	})
}

// statusWriter records the status code sent to the client.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
