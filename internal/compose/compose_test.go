package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const realisticCompose = `
services:
  web:
    image: nginx:1.27
    build: ./web
    dockerfile: Dockerfile.web
    command: sh -c "nginx -g 'daemon off;'"
    entrypoint: ["/docker-entrypoint.sh"]
    environment:
      NGINX_PORT: 80
      NGINX_HOST: example.com
    ports:
      - "8080:80"
      - "443:443/udp"
    volumes:
      - ./site:/usr/share/nginx/html:ro
    depends_on: [api, cache]
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost/"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 1m30s
    user: "101:101"
    working_dir: /srv
    hostname: web1
    cap_add: [NET_BIND_SERVICE]
    cap_drop: [CHOWN]
    mem_limit: 512m
    cpus: 0.5
    pids_limit: 100
    network_mode: bridge
    labels:
      app: demo
      version: "2"
    tty: true
    init: true
  cache:
    image: redis:7
  api:
    image: api:2
    depends_on: [cache]
`

func TestLoadRealistic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, []byte(realisticCompose), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Name != "proj" {
		t.Errorf("project name = %q, want %q", p.Name, "proj")
	}
	if len(p.Services) != 3 || p.Services[0].Name != "api" || p.Services[1].Name != "cache" || p.Services[2].Name != "web" {
		t.Errorf("services = %+v, want names api, cache, web in sorted order", p.Services)
	}
	svc, ok := p.Service("web")
	if !ok {
		t.Fatal("web service not found")
	}
	if svc.Image != "nginx:1.27" || svc.Build != "./web" || svc.Dockerfile != "Dockerfile.web" {
		t.Errorf("image/build = %q/%q/%q", svc.Image, svc.Build, svc.Dockerfile)
	}
	if want := []string{"sh", "-c", "nginx -g 'daemon off;'"}; !reflect.DeepEqual(svc.Command, want) {
		t.Errorf("command = %q, want %q", svc.Command, want)
	}
	if want := []string{"/docker-entrypoint.sh"}; !reflect.DeepEqual(svc.Entrypoint, want) {
		t.Errorf("entrypoint = %q, want %q", svc.Entrypoint, want)
	}
	if want := []string{"NGINX_HOST=example.com", "NGINX_PORT=80"}; !reflect.DeepEqual(svc.Environment, want) {
		t.Errorf("environment = %q, want %q", svc.Environment, want)
	}
	if want := []string{"8080:80", "443:443/udp"}; !reflect.DeepEqual(svc.Ports, want) {
		t.Errorf("ports = %q, want %q", svc.Ports, want)
	}
	if want := []string{"./site:/usr/share/nginx/html:ro"}; !reflect.DeepEqual(svc.Volumes, want) {
		t.Errorf("volumes = %q, want %q", svc.Volumes, want)
	}
	if want := []string{"api", "cache"}; !reflect.DeepEqual(svc.DependsOn, want) {
		t.Errorf("depends_on = %q, want %q", svc.DependsOn, want)
	}
	if svc.Restart != "unless-stopped" {
		t.Errorf("restart = %q, want %q", svc.Restart, "unless-stopped")
	}
	hc := svc.HealthCheck
	if hc == nil {
		t.Fatal("healthcheck missing")
	}
	if want := []string{"CMD", "curl", "-f", "http://localhost/"}; !reflect.DeepEqual(hc.Test, want) {
		t.Errorf("healthcheck test = %q, want %q", hc.Test, want)
	}
	if hc.Interval != 30*time.Second || hc.Timeout != 5*time.Second || hc.Retries != 3 || hc.StartPeriod != 90*time.Second {
		t.Errorf("healthcheck = %+v", hc)
	}
	if svc.User != "101:101" || svc.WorkingDir != "/srv" || svc.Hostname != "web1" {
		t.Errorf("user/working_dir/hostname = %q/%q/%q", svc.User, svc.WorkingDir, svc.Hostname)
	}
	if want := []string{"NET_BIND_SERVICE"}; !reflect.DeepEqual(svc.CapAdd, want) {
		t.Errorf("cap_add = %q, want %q", svc.CapAdd, want)
	}
	if want := []string{"CHOWN"}; !reflect.DeepEqual(svc.CapDrop, want) {
		t.Errorf("cap_drop = %q, want %q", svc.CapDrop, want)
	}
	if svc.Memory != "512m" || svc.CPUs != "0.5" || svc.PidsLimit != 100 || svc.NetworkMode != "bridge" {
		t.Errorf("mem/cpus/pids/network = %q/%q/%d/%q", svc.Memory, svc.CPUs, svc.PidsLimit, svc.NetworkMode)
	}
	if want := map[string]string{"app": "demo", "version": "2"}; !reflect.DeepEqual(svc.Labels, want) {
		t.Errorf("labels = %v, want %v", svc.Labels, want)
	}
	if !svc.Tty || !svc.Init {
		t.Errorf("tty/init = %v/%v, want true/true", svc.Tty, svc.Init)
	}
}

func TestParseEnvironmentForms(t *testing.T) {
	tests := []struct {
		name    string
		compose string
		want    []string
	}{
		{
			name: "map form with numeric and boolean values",
			compose: `
services:
  web:
    image: a
    environment:
      NUMBER: 42
      FLAG: true
      EMPTY:
      QUOTED: "x=y"
`,
			want: []string{"EMPTY=", "FLAG=true", "NUMBER=42", "QUOTED=x=y"},
		},
		{
			name: "list form sorted with last duplicate winning",
			compose: `
services:
  web:
    image: a
    environment:
      - B=2
      - A=1
      - A=3
      - BARE
`,
			want: []string{"A=3", "B=2", "BARE="},
		},
	}
	for _, tt := range tests {
		p, err := Parse([]byte(tt.compose), "test")
		if err != nil {
			t.Errorf("%s: Parse: %v", tt.name, err)
			continue
		}
		if !reflect.DeepEqual(p.Services[0].Environment, tt.want) {
			t.Errorf("%s: environment = %q, want %q", tt.name, p.Services[0].Environment, tt.want)
		}
	}
}

func TestCommandSplitting(t *testing.T) {
	tests := []struct {
		in        string
		want      []string
		wantError string
	}{
		{in: `sh -c "echo hi"`, want: []string{"sh", "-c", "echo hi"}},
		{in: `echo 'a b' c`, want: []string{"echo", "a b", "c"}},
		{in: `a"b c"d`, want: []string{"ab cd"}},
		{in: `echo "escaped \"quote\""`, want: []string{"echo", `escaped "quote"`}},
		{in: `nginx -g 'daemon off;'`, want: []string{"nginx", "-g", "daemon off;"}},
		{in: `unterminated 'quote`, wantError: "unterminated single quote"},
		{in: `unterminated "quote`, wantError: "unterminated double quote"},
	}
	for _, tt := range tests {
		got, err := splitCommand(tt.in)
		if tt.wantError != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Errorf("splitCommand(%q) error = %v, want %q", tt.in, err, tt.wantError)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitCommand(%q): %v", tt.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitCommand(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStartOrderDiamond(t *testing.T) {
	const in = `
services:
  web:
    image: w
    depends_on: [api, cache]
  api:
    image: a
    depends_on: [base]
  cache:
    image: c
    depends_on: [base]
  base:
    image: b
`
	p, err := Parse([]byte(in), "diamond")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	order, err := p.StartOrder()
	if err != nil {
		t.Fatalf("StartOrder: %v", err)
	}
	if len(order) != 4 || order[0] != "base" || order[3] != "web" {
		t.Errorf("StartOrder = %v, want base first and web last", order)
	}
}

func TestStartOrderCycle(t *testing.T) {
	const in = `
services:
  a:
    image: a
    depends_on: [b]
  b:
    image: b
    depends_on: [a]
`
	_, err := Parse([]byte(in), "cyc")
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("Parse cycle error = %v, want it to name the cycle", err)
	}
	p := &Project{Services: []Service{
		{Name: "web", DependsOn: []string{"db"}},
		{Name: "db", DependsOn: []string{"web"}},
	}}
	_, err = p.StartOrder()
	if err == nil || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "web") {
		t.Errorf("StartOrder cycle error = %v, want a cycle naming web", err)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		compose string
		wantErr string
	}{
		{"missing image and build", "services:\n  web:\n    command: sh\n", `service "web"`},
		{"bad port", "services:\n  web:\n    image: a\n    ports:\n      - \"x:y\"\n", "invalid"},
		{"bad memory", "services:\n  web:\n    image: a\n    mem_limit: lots\n", "mem_limit"},
		{"bad cpus", "services:\n  web:\n    image: a\n    cpus: fast\n", "cpus"},
		{"bad restart", "services:\n  web:\n    image: a\n    restart: sometimes\n", "restart policy"},
		{"bad interval", "services:\n  web:\n    image: a\n    healthcheck:\n      test: [CMD]\n      interval: 30\n", "interval"},
		{"unknown depends_on", "services:\n  web:\n    image: a\n    depends_on: [ghost]\n", "unknown service"},
	}
	for _, tt := range tests {
		_, err := Parse([]byte(tt.compose), "test")
		if err == nil {
			t.Errorf("%s: Parse succeeded, want error", tt.name)
			continue
		}
		if !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%s: error = %q, want it to contain %q", tt.name, err, tt.wantErr)
		}
	}
}

func TestParseProjectName(t *testing.T) {
	p, err := Parse([]byte("services:\n  web:\n    image: a\n"), "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "fallback" {
		t.Errorf("project name = %q, want %q", p.Name, "fallback")
	}
	p, err = Parse([]byte("name: fromfile\nservices:\n  web:\n    image: a\n"), "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "fromfile" {
		t.Errorf("project name = %q, want %q", p.Name, "fromfile")
	}
}

func TestHealthCheckStringTest(t *testing.T) {
	const in = `
services:
  web:
    image: a
    healthcheck:
      test: curl -f http://localhost/
      interval: 500ms
`
	p, err := Parse([]byte(in), "t")
	if err != nil {
		t.Fatal(err)
	}
	hc := p.Services[0].HealthCheck
	if hc == nil {
		t.Fatal("healthcheck missing")
	}
	if want := []string{"CMD-SHELL", "curl -f http://localhost/"}; !reflect.DeepEqual(hc.Test, want) {
		t.Errorf("test = %q, want %q", hc.Test, want)
	}
	if hc.Interval != 500*time.Millisecond {
		t.Errorf("interval = %v, want 500ms", hc.Interval)
	}
}

func TestDependsOnMappingForm(t *testing.T) {
	const in = `
services:
  web:
    image: a
    depends_on:
      db:
        condition: service_healthy
  db:
    image: d
`
	p, err := Parse([]byte(in), "t")
	if err != nil {
		t.Fatal(err)
	}
	web, ok := p.Service("web")
	if !ok {
		t.Fatal("web not found")
	}
	if want := []string{"db"}; !reflect.DeepEqual(web.DependsOn, want) {
		t.Errorf("depends_on = %q, want %q", web.DependsOn, want)
	}
}

func TestServiceLookup(t *testing.T) {
	p, err := Parse([]byte("services:\n  web:\n    image: a\n"), "t")
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := p.Service("web"); !ok || s.Image != "a" {
		t.Errorf("Service(web) = %+v, %v", s, ok)
	}
	if _, ok := p.Service("nope"); ok {
		t.Error("Service(nope) should not be found")
	}
}

func TestMalformedYAML(t *testing.T) {
	for _, in := range []string{
		"services: [unclosed\n",
		"services: 5\n",
		"services:\n  web: 5\n",
		"services:\n  web:\n    image: [a, b\n",
	} {
		_, err := Parse([]byte(in), "t")
		if err == nil {
			t.Errorf("Parse(%q) succeeded, want error", in)
		}
	}
}

func TestBuildMappingForm(t *testing.T) {
	const in = `
services:
  web:
    build:
      context: ./src
      dockerfile: Dockerfile.dev
`
	p, err := Parse([]byte(in), "t")
	if err != nil {
		t.Fatal(err)
	}
	if p.Services[0].Build != "./src" || p.Services[0].Dockerfile != "Dockerfile.dev" {
		t.Errorf("build = %q/%q, want ./src/Dockerfile.dev", p.Services[0].Build, p.Services[0].Dockerfile)
	}
}

func TestPortsIntForm(t *testing.T) {
	const in = `
services:
  web:
    image: a
    ports:
      - 8080
      - "9090:80"
`
	p, err := Parse([]byte(in), "t")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"8080", "9090:80"}; !reflect.DeepEqual(p.Services[0].Ports, want) {
		t.Errorf("ports = %q, want %q", p.Services[0].Ports, want)
	}
}
