package api

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoggingMiddleware(t *testing.T) {
	var buf bytes.Buffer
	h := withLogging(&buf, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/containers/create?name=x", nil))
	if !strings.Contains(buf.String(), "POST /containers/create 418") {
		t.Errorf("log = %q, want method, path and status", buf.String())
	}
}

func TestLoggingMiddlewareDefaultStatus(t *testing.T) {
	var buf bytes.Buffer
	h := withLogging(&buf, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/_ping", nil))
	if !strings.Contains(buf.String(), "GET /_ping 200") {
		t.Errorf("log = %q, want method, path and status 200", buf.String())
	}
}

func TestServeUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "minibox.sock")
	if err := os.WriteFile(sock, []byte("stale"), 0o644); err != nil {
		t.Fatalf("create stale socket file: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, sock, "", &fakeBackend{}) }()

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	var resp *http.Response
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; {
		resp, err = client.Get("http://minibox/_ping")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("socket never became ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer resp.Body.Close()
	wantStatus(t, resp, http.StatusOK)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read ping body: %v", err)
	}
	if string(body) != "OK" {
		t.Errorf("body = %q, want OK", body)
	}
	if got := resp.Header.Get("Api-Version"); got != apiVersion {
		t.Errorf("Api-Version header = %q, want %q", got, apiVersion)
	}
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if fi.Mode().Perm() != 0o660 {
		t.Errorf("socket mode = %v, want 660", fi.Mode().Perm())
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Errorf("socket still present after shutdown (err=%v)", err)
	}
}

func TestServeTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, filepath.Join(t.TempDir(), "also.sock"), addr, &fakeBackend{})
	}()

	var resp *http.Response
	for deadline := time.Now().Add(10 * time.Second); ; {
		resp, err = http.Get("http://" + addr + "/_ping")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tcp endpoint never became ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp.Body.Close()
	wantStatus(t, resp, http.StatusOK)

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
}
