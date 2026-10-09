package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// fakeBackend implements Backend with canned data and call recording.
type fakeBackend struct {
	version    VersionInfo
	containers []ContainerSummary
	images     []ImageSummary
	inspect    string
	logsData   string
	pullSteps  []string
	pullErr    error
	waitCode   int
	warnings   []string
	err        error

	calls        []string
	createReq    CreateRequest
	stopTimeout  int
	killSignal   string
	removeForce  bool
	pullRef      string
	lastAll      bool
	lastFollow   bool
	lastTail     int
	inspectedCtr string
	inspectedImg string
}

func (f *fakeBackend) Ping(ctx context.Context) error {
	f.calls = append(f.calls, "ping")
	return f.err
}

func (f *fakeBackend) Version(ctx context.Context) (VersionInfo, error) {
	f.calls = append(f.calls, "version")
	return f.version, f.err
}

func (f *fakeBackend) ListContainers(ctx context.Context, all bool) ([]ContainerSummary, error) {
	f.calls = append(f.calls, "list")
	f.lastAll = all
	return f.containers, f.err
}

func (f *fakeBackend) InspectContainer(ctx context.Context, id string) (json.RawMessage, error) {
	f.calls = append(f.calls, "inspect "+id)
	f.inspectedCtr = id
	if f.err != nil {
		return nil, f.err
	}
	return json.RawMessage(f.inspect), nil
}

func (f *fakeBackend) CreateContainer(ctx context.Context, req CreateRequest) (CreateResponse, error) {
	f.calls = append(f.calls, "create")
	f.createReq = req
	if f.err != nil {
		return CreateResponse{}, f.err
	}
	return CreateResponse{ID: "c123", Warnings: f.warnings}, nil
}

func (f *fakeBackend) StartContainer(ctx context.Context, id string) error {
	f.calls = append(f.calls, "start "+id)
	return f.err
}

func (f *fakeBackend) StopContainer(ctx context.Context, id string, timeoutSeconds int) error {
	f.calls = append(f.calls, "stop "+id)
	f.stopTimeout = timeoutSeconds
	return f.err
}

func (f *fakeBackend) KillContainer(ctx context.Context, id, signal string) error {
	f.calls = append(f.calls, "kill "+id)
	f.killSignal = signal
	return f.err
}

func (f *fakeBackend) RemoveContainer(ctx context.Context, id string, force bool) error {
	f.calls = append(f.calls, "remove "+id)
	f.removeForce = force
	return f.err
}

func (f *fakeBackend) WaitContainer(ctx context.Context, id string) (int, error) {
	f.calls = append(f.calls, "wait "+id)
	return f.waitCode, f.err
}

func (f *fakeBackend) ContainerLogs(ctx context.Context, id string, follow bool, tail int) (io.ReadCloser, error) {
	f.calls = append(f.calls, "logs "+id)
	f.lastFollow = follow
	f.lastTail = tail
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader(f.logsData)), nil
}

func (f *fakeBackend) ListImages(ctx context.Context) ([]ImageSummary, error) {
	f.calls = append(f.calls, "images")
	return f.images, f.err
}

func (f *fakeBackend) InspectImage(ctx context.Context, name string) (json.RawMessage, error) {
	f.calls = append(f.calls, "inspect image "+name)
	f.inspectedImg = name
	if f.err != nil {
		return nil, f.err
	}
	return json.RawMessage(f.inspect), nil
}

func (f *fakeBackend) PullImage(ctx context.Context, ref string, progress func(string)) error {
	f.calls = append(f.calls, "pull "+ref)
	f.pullRef = ref
	for _, step := range f.pullSteps {
		progress(step)
	}
	return f.pullErr
}

func newTestServer(t *testing.T, b *fakeBackend) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(NewHandler(b))
	t.Cleanup(ts.Close)
	return ts
}

func request(t *testing.T, ts *httptest.Server, method, path string, body io.Reader) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s body: %v", method, path, err)
	}
	return resp, string(data)
}

func decodeObject(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode %q as object: %v", body, err)
	}
	return m
}

func wantStatus(t *testing.T, resp *http.Response, code int) {
	t.Helper()
	if resp.StatusCode != code {
		t.Fatalf("status = %d, want %d", resp.StatusCode, code)
	}
}

func wantMessage(t *testing.T, body, substr string) {
	t.Helper()
	m := decodeObject(t, body)
	msg, _ := m["message"].(string)
	if !strings.Contains(msg, substr) {
		t.Errorf("message = %q, want it to contain %q", msg, substr)
	}
}

func TestPing(t *testing.T) {
	ts := newTestServer(t, &fakeBackend{})
	resp, body := request(t, ts, "GET", "/_ping", nil)
	wantStatus(t, resp, http.StatusOK)
	if body != "OK" {
		t.Errorf("body = %q, want %q", body, "OK")
	}
	if got := resp.Header.Get("Api-Version"); got != apiVersion {
		t.Errorf("Api-Version header = %q, want %q", got, apiVersion)
	}
	if got := resp.Header.Get("Docker-Experimental"); got != "false" {
		t.Errorf("Docker-Experimental header = %q, want false", got)
	}
}

func TestPingBackendError(t *testing.T) {
	ts := newTestServer(t, &fakeBackend{err: errors.New("storage offline")})
	resp, body := request(t, ts, "GET", "/_ping", nil)
	wantStatus(t, resp, http.StatusInternalServerError)
	wantMessage(t, body, "storage offline")
}

func TestVersion(t *testing.T) {
	b := &fakeBackend{version: VersionInfo{
		Version: "24.0.2", APIVersion: "1.43", GitCommit: "deadbeef",
		GoVersion: "go1.23", Os: "linux", Arch: "amd64", KernelVersion: "6.1.0",
	}}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "GET", "/version", nil)
	wantStatus(t, resp, http.StatusOK)
	m := decodeObject(t, body)
	if m["Version"] != "24.0.2" || m["ApiVersion"] != "1.43" || m["Os"] != "linux" {
		t.Errorf("version body = %v", m)
	}
	if _, ok := m["GitCommit"]; !ok {
		t.Errorf("GitCommit missing from %v", m)
	}
}

func TestInfoCounts(t *testing.T) {
	b := &fakeBackend{
		version: VersionInfo{Version: "24.0.2"},
		containers: []ContainerSummary{
			{ID: "c1", State: "running"},
			{ID: "c2", State: "running"},
			{ID: "c3", State: "exited"},
		},
		images: []ImageSummary{{ID: "i1"}, {ID: "i2"}},
	}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "GET", "/info", nil)
	wantStatus(t, resp, http.StatusOK)
	m := decodeObject(t, body)
	for key, want := range map[string]float64{
		"Containers": 3, "ContainersRunning": 2, "ContainersStopped": 1, "Images": 2,
	} {
		got, ok := m[key].(float64)
		if !ok {
			t.Fatalf("info missing %s: %v", key, m)
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

func TestContainersListEmpty(t *testing.T) {
	ts := newTestServer(t, &fakeBackend{})
	resp, body := request(t, ts, "GET", "/containers/json", nil)
	wantStatus(t, resp, http.StatusOK)
	if strings.TrimSpace(body) != "[]" {
		t.Errorf("body = %q, want []", body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestContainersListFields(t *testing.T) {
	b := &fakeBackend{containers: []ContainerSummary{{
		ID: "c1", Names: []string{"/web"}, Image: "alpine", Command: "sleep 5",
		Created: 1700000000, State: "running", Status: "Up 2 minutes",
		Ports:  []PortSummary{{IP: "127.0.0.1", PrivatePort: 80, PublicPort: 8080, Type: "tcp"}},
		Labels: map[string]string{"app": "web"},
	}}}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "GET", "/containers/json?all=1", nil)
	wantStatus(t, resp, http.StatusOK)
	if !b.lastAll {
		t.Error("backend received all=false, want true")
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if len(list) != 1 {
		t.Fatalf("list length = %d, want 1", len(list))
	}
	first := list[0]
	for _, key := range []string{"Id", "Names", "State", "Image", "Status", "Ports", "Labels"} {
		if _, ok := first[key]; !ok {
			t.Errorf("summary missing %q field", key)
		}
	}
	if _, ok := first["ID"]; ok {
		t.Error(`summary uses "ID", want Docker's "Id"`)
	}
	if first["Id"] != "c1" || first["State"] != "running" {
		t.Errorf("Id/State = %v/%v, want c1/running", first["Id"], first["State"])
	}
}

const createBody = `{
	"Image": "alpine:3.19",
	"Cmd": ["echo", "hi"],
	"Entrypoint": ["/bin/sh", "-c"],
	"Env": ["FOO=bar"],
	"WorkingDir": "/work",
	"User": "1000:1000",
	"Hostname": "test",
	"Tty": true,
	"OpenStdin": true,
	"Labels": {"a": "b"},
	"Bogus": {"ignored": true},
	"HostConfig": {
		"Binds": ["/tmp:/tmp"],
		"Memory": 1048576,
		"NanoCpus": 500000000,
		"PidsLimit": 100,
		"AutoRemove": true,
		"ReadonlyRootfs": true,
		"NetworkMode": "bridge",
		"CapAdd": ["NET_ADMIN"],
		"CapDrop": ["MKNOD"],
		"Tmpfs": {"/run": "rw"},
		"RestartPolicy": {"Name": "on-failure", "MaximumRetryCount": 3},
		"PortBindings": {"80/tcp": [{"HostIp": "127.0.0.1", "HostPort": "8080"}]}
	}
}`

func TestCreateMapsRequest(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/containers/create?name=web", strings.NewReader(createBody))
	wantStatus(t, resp, http.StatusCreated)
	m := decodeObject(t, body)
	if m["Id"] != "c123" {
		t.Errorf("Id = %v, want c123", m["Id"])
	}
	if _, ok := m["Warnings"]; ok {
		t.Error("Warnings present, want omitted when empty")
	}
	pids := int64(100)
	want := CreateRequest{
		Image: "alpine:3.19", Cmd: []string{"echo", "hi"},
		Entrypoint: []string{"/bin/sh", "-c"}, Env: []string{"FOO=bar"},
		WorkingDir: "/work", User: "1000:1000", Hostname: "test",
		Tty: true, OpenStdin: true, Labels: map[string]string{"a": "b"},
		HostConfig: HostConfig{
			Binds: []string{"/tmp:/tmp"}, Memory: 1048576, NanoCPUs: 500000000,
			PidsLimit: &pids, AutoRemove: true, ReadonlyRootfs: true,
			NetworkMode: "bridge", CapAdd: []string{"NET_ADMIN"}, CapDrop: []string{"MKNOD"},
			Tmpfs:         map[string]string{"/run": "rw"},
			RestartPolicy: RestartPolicy{Name: "on-failure", MaximumRetryCount: 3},
			PortBindings:  map[string][]PortBinding{"80/tcp": {{HostIP: "127.0.0.1", HostPort: "8080"}}},
		},
	}
	if !reflect.DeepEqual(b.createReq, want) {
		t.Errorf("backend request =\n%+v\nwant\n%+v", b.createReq, want)
	}
}

func TestCreateWarnings(t *testing.T) {
	b := &fakeBackend{warnings: []string{"blkid failed"}}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/containers/create", strings.NewReader("{}"))
	wantStatus(t, resp, http.StatusCreated)
	ws, ok := decodeObject(t, body)["Warnings"].([]any)
	if !ok || len(ws) != 1 {
		t.Errorf("Warnings missing or wrong: %s", body)
	}
}

func TestCreateMissingImage(t *testing.T) {
	b := &fakeBackend{err: errors.New("no such image: alpine:3.19")}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/containers/create", strings.NewReader(`{"Image":"alpine:3.19"}`))
	wantStatus(t, resp, http.StatusNotFound)
	wantMessage(t, body, "no such image")
}

func TestCreateOtherBackendError(t *testing.T) {
	b := &fakeBackend{err: errors.New("storage full")}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/containers/create", strings.NewReader(`{"Image":"alpine"}`))
	wantStatus(t, resp, http.StatusInternalServerError)
	wantMessage(t, body, "storage full")
}

func TestCreateMalformedJSON(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/containers/create", strings.NewReader(`{"Image":`))
	wantStatus(t, resp, http.StatusBadRequest)
	wantMessage(t, body, "invalid JSON")
	if len(b.calls) != 0 {
		t.Errorf("backend called with malformed body: %v", b.calls)
	}
}

func TestStart(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/containers/c1/start", nil)
	wantStatus(t, resp, http.StatusNoContent)
	if body != "" {
		t.Errorf("204 body = %q, want empty", body)
	}
	if len(b.calls) != 1 || b.calls[0] != "start c1" {
		t.Errorf("calls = %v, want [start c1]", b.calls)
	}
}

func TestStartNotFound(t *testing.T) {
	b := &fakeBackend{err: errors.New("no such container: c1")}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/containers/c1/start", nil)
	wantStatus(t, resp, http.StatusNotFound)
	wantMessage(t, body, "no such container")
}

func TestStopTimeout(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, _ := request(t, ts, "POST", "/containers/c1/stop?t=7", nil)
	wantStatus(t, resp, http.StatusNoContent)
	if b.stopTimeout != 7 {
		t.Errorf("stop timeout = %d, want 7", b.stopTimeout)
	}
}

func TestStopDefaultTimeout(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, _ := request(t, ts, "POST", "/containers/c1/stop", nil)
	wantStatus(t, resp, http.StatusNoContent)
	if b.stopTimeout != defaultStopTimeout {
		t.Errorf("stop timeout = %d, want %d", b.stopTimeout, defaultStopTimeout)
	}
}

func TestStopInvalidTimeout(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/containers/c1/stop?t=soon", nil)
	wantStatus(t, resp, http.StatusBadRequest)
	wantMessage(t, body, "invalid timeout")
	if len(b.calls) != 0 {
		t.Errorf("backend called with invalid timeout: %v", b.calls)
	}
}

func TestKillSignal(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, _ := request(t, ts, "POST", "/containers/c1/kill?signal=USR1", nil)
	wantStatus(t, resp, http.StatusNoContent)
	if b.killSignal != "USR1" {
		t.Errorf("kill signal = %q, want USR1", b.killSignal)
	}
}

func TestKillDefaultSignal(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, _ := request(t, ts, "POST", "/containers/c1/kill", nil)
	wantStatus(t, resp, http.StatusNoContent)
	if b.killSignal != "KILL" {
		t.Errorf("kill signal = %q, want KILL", b.killSignal)
	}
}

func TestRestartStopsThenStarts(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, _ := request(t, ts, "POST", "/containers/c1/restart?t=3", nil)
	wantStatus(t, resp, http.StatusNoContent)
	wantCalls := []string{"stop c1", "start c1"}
	if !reflect.DeepEqual(b.calls, wantCalls) {
		t.Errorf("calls = %v, want %v", b.calls, wantCalls)
	}
	if b.stopTimeout != 3 {
		t.Errorf("restart timeout = %d, want 3", b.stopTimeout)
	}
}

func TestDeleteForce(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, _ := request(t, ts, "DELETE", "/containers/c1?force=1", nil)
	wantStatus(t, resp, http.StatusNoContent)
	if !b.removeForce {
		t.Error("force = false, want true")
	}
	b2 := &fakeBackend{}
	ts2 := newTestServer(t, b2)
	resp, _ = request(t, ts2, "DELETE", "/containers/c1", nil)
	wantStatus(t, resp, http.StatusNoContent)
	if b2.removeForce {
		t.Error("force = true, want false")
	}
}

func TestInspectContainer(t *testing.T) {
	b := &fakeBackend{inspect: `{"Id":"c1","Name":"/web"}`}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "GET", "/containers/c1/json", nil)
	wantStatus(t, resp, http.StatusOK)
	if body != b.inspect {
		t.Errorf("body = %q, want %q", body, b.inspect)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if b.inspectedCtr != "c1" {
		t.Errorf("inspected id = %q, want c1", b.inspectedCtr)
	}
}

func TestInspectContainerNotFound(t *testing.T) {
	b := &fakeBackend{err: errors.New("no such container: nope")}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "GET", "/containers/nope/json", nil)
	wantStatus(t, resp, http.StatusNotFound)
	wantMessage(t, body, "no such container")
}

func TestWait(t *testing.T) {
	b := &fakeBackend{waitCode: 3}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/containers/c1/wait", nil)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeObject(t, body)["StatusCode"].(float64); got != 3 {
		t.Errorf("StatusCode = %v, want 3", got)
	}
}

func TestLogsStream(t *testing.T) {
	b := &fakeBackend{logsData: "line one\nline two\n"}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "GET", "/containers/c1/logs", nil)
	wantStatus(t, resp, http.StatusOK)
	if body != b.logsData {
		t.Errorf("body = %q, want %q", body, b.logsData)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
	if b.lastFollow {
		t.Error("follow = true, want false")
	}
	if b.lastTail != -1 {
		t.Errorf("tail = %d, want -1 (all)", b.lastTail)
	}
}

func TestLogsFollowAndTail(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, _ := request(t, ts, "GET", "/containers/c1/logs?follow=1&tail=5", nil)
	wantStatus(t, resp, http.StatusOK)
	if !b.lastFollow {
		t.Error("follow = false, want true")
	}
	if b.lastTail != 5 {
		t.Errorf("tail = %d, want 5", b.lastTail)
	}
}

func TestLogsTailZeroAndAll(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, _ := request(t, ts, "GET", "/containers/c1/logs?tail=0", nil)
	wantStatus(t, resp, http.StatusOK)
	if b.lastTail != 0 {
		t.Errorf("tail = %d, want 0", b.lastTail)
	}
	b2 := &fakeBackend{}
	ts2 := newTestServer(t, b2)
	resp, _ = request(t, ts2, "GET", "/containers/c1/logs?tail=all", nil)
	wantStatus(t, resp, http.StatusOK)
	if b2.lastTail != -1 {
		t.Errorf("tail = %d, want -1", b2.lastTail)
	}
}

func TestLogsInvalidTail(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "GET", "/containers/c1/logs?tail=-2", nil)
	wantStatus(t, resp, http.StatusBadRequest)
	wantMessage(t, body, "invalid tail")
	if len(b.calls) != 0 {
		t.Errorf("backend called with invalid tail: %v", b.calls)
	}
}

func TestImagesList(t *testing.T) {
	b := &fakeBackend{images: []ImageSummary{{
		ID: "sha256:abc", RepoTags: []string{"alpine:3.19"},
		Created: 1700000000, Size: 7000000, VirtualSize: 7000000,
	}}}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "GET", "/images/json", nil)
	wantStatus(t, resp, http.StatusOK)
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if len(list) != 1 {
		t.Fatalf("list length = %d, want 1", len(list))
	}
	if list[0]["Id"] != "sha256:abc" {
		t.Errorf("Id = %v, want sha256:abc", list[0]["Id"])
	}
	if _, ok := list[0]["RepoTags"]; !ok {
		t.Error("RepoTags missing")
	}
}

func TestImageInspectNestedName(t *testing.T) {
	b := &fakeBackend{inspect: `{"Id":"sha256:abc"}`}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "GET", "/images/library/alpine:3.19/json", nil)
	wantStatus(t, resp, http.StatusOK)
	if body != b.inspect {
		t.Errorf("body = %q, want %q", body, b.inspect)
	}
	if b.inspectedImg != "library/alpine:3.19" {
		t.Errorf("inspected name = %q, want library/alpine:3.19", b.inspectedImg)
	}
}

func TestImageInspectNotFound(t *testing.T) {
	b := &fakeBackend{err: errors.New("no such image: ghost")}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "GET", "/images/ghost/json", nil)
	wantStatus(t, resp, http.StatusNotFound)
	wantMessage(t, body, "no such image")
}

func TestPullStreamComplete(t *testing.T) {
	b := &fakeBackend{pullSteps: []string{"Pulling from library/alpine", "Download complete"}}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/images/create?fromImage=alpine&tag=3.19", nil)
	wantStatus(t, resp, http.StatusOK)
	if b.pullRef != "alpine:3.19" {
		t.Errorf("pull ref = %q, want alpine:3.19", b.pullRef)
	}
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d stream lines: %q", len(lines), body)
	}
	var statuses []string
	for i, line := range lines {
		m := decodeObject(t, line)
		s, ok := m["status"].(string)
		if !ok {
			t.Fatalf("line %d lacks status: %v", i, m)
		}
		statuses = append(statuses, s)
	}
	want := []string{"Pulling from library/alpine", "Download complete", "complete"}
	if !reflect.DeepEqual(statuses, want) {
		t.Errorf("statuses = %v, want %v", statuses, want)
	}
}

func TestPullStreamError(t *testing.T) {
	b := &fakeBackend{pullSteps: []string{"Pulling fs layer"}, pullErr: errors.New("connection refused")}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/images/create?fromImage=alpine", nil)
	wantStatus(t, resp, http.StatusOK)
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d stream lines: %q", len(lines), body)
	}
	last := decodeObject(t, lines[1])
	if last["error"] != "connection refused" {
		t.Errorf("last line = %v, want error connection refused", last)
	}
	if _, ok := decodeObject(t, lines[0])["status"]; !ok {
		t.Errorf("first line = %q, want a status line", lines[0])
	}
}

func TestPullMissingFromImage(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/images/create", nil)
	wantStatus(t, resp, http.StatusBadRequest)
	wantMessage(t, body, "fromImage")
	if b.pullRef != "" {
		t.Errorf("backend called with ref %q", b.pullRef)
	}
}

func TestUnknownRoute(t *testing.T) {
	ts := newTestServer(t, &fakeBackend{})
	for _, path := range []string{"/", "/nope", "/containers/c1/bogus", "/images/bogus"} {
		resp, body := request(t, ts, "GET", path, nil)
		wantStatus(t, resp, http.StatusNotFound)
		wantMessage(t, body, "page not found")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	ts := newTestServer(t, &fakeBackend{})
	cases := []struct{ method, path string }{
		{"POST", "/_ping"},
		{"DELETE", "/version"},
		{"POST", "/containers/json"},
		{"GET", "/containers/create"},
		{"GET", "/containers/c1/start"},
		{"PUT", "/containers/c1"},
		{"GET", "/images/create"},
	}
	for _, tc := range cases {
		resp, body := request(t, ts, tc.method, tc.path, nil)
		wantStatus(t, resp, http.StatusMethodNotAllowed)
		wantMessage(t, body, "not allowed")
	}
}

func TestCreateHugeBody(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	big := strings.Repeat("a", 5<<20)
	resp, _ := request(t, ts, "POST", "/containers/create", strings.NewReader(`{"Image":"`+big+`","Cmd":["x"]}`))
	wantStatus(t, resp, http.StatusBadRequest)
	if len(b.calls) != 0 {
		t.Errorf("backend called with oversized body: %v", b.calls)
	}
}

func TestCreateOddBody(t *testing.T) {
	b := &fakeBackend{}
	ts := newTestServer(t, b)
	resp, body := request(t, ts, "POST", "/containers/create", strings.NewReader("\x00\xff\xfe\x01"))
	wantStatus(t, resp, http.StatusBadRequest)
	wantMessage(t, body, "invalid JSON")
	if len(b.calls) != 0 {
		t.Errorf("backend called with binary body: %v", b.calls)
	}
}
