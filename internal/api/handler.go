package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// apiVersion is the Docker Engine API version the server advertises.
const apiVersion = "1.47"

// defaultStopTimeout is Docker's default stop grace period in seconds.
const defaultStopTimeout = 10

// maxCreateBody bounds the JSON body accepted by /containers/create.
const maxCreateBody = 4 << 20

// handler serves the Docker-compatible routes over a Backend.
type handler struct {
	backend Backend
}

// NewHandler returns the HTTP handler for the backend.
func NewHandler(b Backend) http.Handler {
	h := &handler{backend: b}
	mux := http.NewServeMux()
	mux.HandleFunc("/_ping", h.ping)
	mux.HandleFunc("/version", h.version)
	mux.HandleFunc("/info", h.info)
	mux.HandleFunc("/containers/json", h.listContainers)
	mux.HandleFunc("/containers/create", h.createContainer)
	mux.HandleFunc("/containers/{id}/start", h.startContainer)
	mux.HandleFunc("/containers/{id}/stop", h.stopContainer)
	mux.HandleFunc("/containers/{id}/kill", h.killContainer)
	mux.HandleFunc("/containers/{id}/restart", h.restartContainer)
	mux.HandleFunc("/containers/{id}", h.deleteContainer)
	mux.HandleFunc("/containers/{id}/json", h.inspectContainer)
	mux.HandleFunc("/containers/{id}/wait", h.waitContainer)
	mux.HandleFunc("/containers/{id}/logs", h.containerLogs)
	mux.HandleFunc("/images/", h.images)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("API-Version", apiVersion)
		// An empty pattern means no route matched; report it as Docker's
		// JSON error instead of net/http's plain-text 404.
		if _, pattern := mux.Handler(r); pattern == "" {
			writeError(w, http.StatusNotFound, "page not found")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (h *handler) ping(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if err := h.backend.Ping(r.Context()); err != nil {
		writeBackendError(w, err)
		return
	}
	w.Header().Set("Docker-Experimental", "false")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

func (h *handler) version(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	v, err := h.backend.Version(r.Context())
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// infoResponse is the minimal /info object Docker-shaped clients expect.
type infoResponse struct {
	ID                string `json:"Id"`
	Name              string `json:"Name"`
	ServerVersion     string `json:"ServerVersion"`
	OSType            string `json:"OSType"`
	Containers        int    `json:"Containers"`
	ContainersRunning int    `json:"ContainersRunning"`
	ContainersPaused  int    `json:"ContainersPaused"`
	ContainersStopped int    `json:"ContainersStopped"`
	Images            int    `json:"Images"`
}

func (h *handler) info(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	ctx := r.Context()
	containers, err := h.backend.ListContainers(ctx, true)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	images, err := h.backend.ListImages(ctx)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	v, err := h.backend.Version(ctx)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	resp := infoResponse{
		ID:            "minibox",
		Name:          "minibox",
		ServerVersion: v.Version,
		OSType:        "linux",
		Images:        len(images),
	}
	for _, c := range containers {
		resp.Containers++
		switch c.State {
		case "running":
			resp.ContainersRunning++
		case "paused":
			resp.ContainersPaused++
		default:
			resp.ContainersStopped++
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) listContainers(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	cs, err := h.backend.ListContainers(r.Context(), queryBool(r, "all"))
	if err != nil {
		writeBackendError(w, err)
		return
	}
	if cs == nil {
		cs = []ContainerSummary{}
	}
	writeJSON(w, http.StatusOK, cs)
}

func (h *handler) createContainer(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req CreateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCreateBody))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	resp, err := h.backend.CreateContainer(r.Context(), req)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (h *handler) startContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := h.backend.StartContainer(r.Context(), id); err != nil {
		writeBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) stopContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	timeout, ok := parseTimeout(w, r)
	if !ok {
		return
	}
	if err := h.backend.StopContainer(r.Context(), id, timeout); err != nil {
		writeBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) killContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	// Docker defaults to SIGKILL when no signal is given.
	signal := r.URL.Query().Get("signal")
	if signal == "" {
		signal = "KILL"
	}
	if err := h.backend.KillContainer(r.Context(), id, signal); err != nil {
		writeBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) restartContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	timeout, ok := parseTimeout(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := h.backend.StopContainer(ctx, id, timeout); err != nil {
		writeBackendError(w, err)
		return
	}
	if err := h.backend.StartContainer(ctx, id); err != nil {
		writeBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) deleteContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireMethod(w, r, http.MethodDelete) {
		return
	}
	if err := h.backend.RemoveContainer(r.Context(), id, queryBool(r, "force")); err != nil {
		writeBackendError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) inspectContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	raw, err := h.backend.InspectContainer(r.Context(), id)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

type waitResponse struct {
	StatusCode int `json:"StatusCode"`
}

func (h *handler) waitContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	code, err := h.backend.WaitContainer(r.Context(), id)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, waitResponse{StatusCode: code})
}

// containerLogs streams the container's log as raw bytes: minibox stores
// plain-text logs, so unlike Docker there is no stream framing and no header
// per log entry.
func (h *handler) containerLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	tail, ok := parseTail(w, r)
	if !ok {
		return
	}
	rc, err := h.backend.ContainerLogs(r.Context(), id, queryBool(r, "follow"), tail)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// Copy in chunks and flush each one so follow-mode logs appear live.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := rc.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// images dispatches under /images/: image names may contain slashes
// (library/alpine), which ServeMux wildcards cannot capture, so the
// remaining path is parsed by hand.
func (h *handler) images(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/images/")
	switch {
	case rest == "json":
		h.listImages(w, r)
	case rest == "create":
		h.pullImage(w, r)
	case strings.HasSuffix(rest, "/json") && len(rest) > len("/json"):
		h.inspectImage(w, r, strings.TrimSuffix(rest, "/json"))
	default:
		writeError(w, http.StatusNotFound, "page not found")
	}
}

func (h *handler) listImages(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	images, err := h.backend.ListImages(r.Context())
	if err != nil {
		writeBackendError(w, err)
		return
	}
	if images == nil {
		images = []ImageSummary{}
	}
	writeJSON(w, http.StatusOK, images)
}

func (h *handler) inspectImage(w http.ResponseWriter, r *http.Request, name string) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	raw, err := h.backend.InspectImage(r.Context(), name)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(raw)
}

// pullImage streams one JSON line per progress message and ends with
// {"status":"complete"}; an error mid-stream is reported as {"error":"..."}
// followed by closing the stream.
func (h *handler) pullImage(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	q := r.URL.Query()
	name := q.Get("fromImage")
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing fromImage parameter")
		return
	}
	ref := name
	if tag := q.Get("tag"); tag != "" {
		ref += ":" + tag
	}
	if digest := q.Get("digest"); digest != "" {
		ref += "@" + digest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeLine := func(v any) {
		data, err := json.Marshal(v)
		if err != nil {
			return
		}
		w.Write(append(data, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}
	if err := h.backend.PullImage(r.Context(), ref, func(msg string) {
		if msg != "" {
			writeLine(map[string]string{"status": msg})
		}
	}); err != nil {
		writeLine(map[string]string{"error": err.Error()})
		return
	}
	writeLine(map[string]string{"status": "complete"})
}

// requireMethod replies with a 405 JSON error unless the request uses method.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	writeError(w, http.StatusMethodNotAllowed,
		fmt.Sprintf("method %s not allowed for %s, use %s", r.Method, r.URL.Path, method))
	return false
}

// parseTimeout reads the ?t= grace period, defaulting to defaultStopTimeout.
func parseTimeout(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("t")
	if raw == "" {
		return defaultStopTimeout, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid timeout %q: must be a non-negative integer", raw))
		return 0, false
	}
	return n, true
}

// parseTail reads ?tail=, a line count from the container's end; "all" or no
// parameter means everything, reported to the backend as a negative tail.
func parseTail(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("tail")
	if raw == "" || raw == "all" {
		return -1, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("invalid tail %q: must be a non-negative integer or \"all\"", raw))
		return 0, false
	}
	return n, true
}

// queryBool parses a Docker-style boolean query parameter (1, t, true, y, yes, on).
func queryBool(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	}
	return false
}

type errorResponse struct {
	Message string `json:"message"`
}

// writeJSON sends v as JSON with the given status.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// writeError replies with Docker's {"message": ...} error shape.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errorResponse{Message: msg})
}

// writeBackendError maps a backend error to a status: not-found-shaped
// errors become 404, everything else 500.
func writeBackendError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	if isNotFound(err) {
		code = http.StatusNotFound
	}
	writeError(w, code, err.Error())
}

// isNotFound reports whether err reads like a missing resource.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such")
}
