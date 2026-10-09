// Package compose parses a small subset of Docker Compose files into a
// Project that minibox can turn into containers.
package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/AlvinPlayz23/minibox/internal/cgroup"
	"github.com/AlvinPlayz23/minibox/internal/network"
)

// HealthCheck mirrors a compose healthcheck block.
type HealthCheck struct {
	Test        []string // e.g. ["CMD-SHELL", "curl -f http://localhost/"]
	Interval    time.Duration
	Timeout     time.Duration
	Retries     int
	StartPeriod time.Duration
}

// Service is one service of a compose file.
type Service struct {
	Name        string
	Image       string
	Build       string // context path
	Dockerfile  string // dockerfile path (default "Dockerfile")
	Command     []string
	Entrypoint  []string
	Environment []string // normalised "KEY=value" entries, sorted
	Ports       []string // raw -p style strings
	Volumes     []string // raw -v style strings
	DependsOn   []string
	Restart     string // no, on-failure, always, unless-stopped
	HealthCheck *HealthCheck
	User        string
	WorkingDir  string
	Hostname    string
	CapAdd      []string
	CapDrop     []string
	Memory      string // raw value such as "512m"
	CPUs        string // raw value such as "0.5"
	PidsLimit   int64
	NetworkMode string
	Labels      map[string]string
	Tty         bool
	Init        bool
}

// Project is a parsed compose file.
type Project struct {
	Name     string
	Services []Service
}

type composeFile struct {
	Name     string               `yaml:"name"`
	Services map[string]yaml.Node `yaml:"services"`
}

// Fields whose YAML shape varies (string vs list vs mapping) are decoded as
// nodes and interpreted by hand; unknown keys are ignored by default.
type rawService struct {
	Image       string          `yaml:"image"`
	Build       yaml.Node       `yaml:"build"`
	Dockerfile  string          `yaml:"dockerfile"`
	Command     yaml.Node       `yaml:"command"`
	Entrypoint  yaml.Node       `yaml:"entrypoint"`
	Environment yaml.Node       `yaml:"environment"`
	Ports       yaml.Node       `yaml:"ports"`
	Volumes     yaml.Node       `yaml:"volumes"`
	DependsOn   yaml.Node       `yaml:"depends_on"`
	Restart     string          `yaml:"restart"`
	HealthCheck *rawHealthCheck `yaml:"healthcheck"`
	User        string          `yaml:"user"`
	WorkingDir  string          `yaml:"working_dir"`
	Hostname    string          `yaml:"hostname"`
	CapAdd      []string        `yaml:"cap_add"`
	CapDrop     []string        `yaml:"cap_drop"`
	Memory      yaml.Node       `yaml:"mem_limit"`
	CPUs        yaml.Node       `yaml:"cpus"`
	PidsLimit   int64           `yaml:"pids_limit"`
	NetworkMode string          `yaml:"network_mode"`
	Labels      yaml.Node       `yaml:"labels"`
	Tty         bool            `yaml:"tty"`
	Init        bool            `yaml:"init"`
}

type rawHealthCheck struct {
	Test        yaml.Node `yaml:"test"`
	Interval    yaml.Node `yaml:"interval"`
	Timeout     yaml.Node `yaml:"timeout"`
	Retries     int       `yaml:"retries"`
	StartPeriod yaml.Node `yaml:"start_period"`
}

// Load reads and parses a compose file. Unknown top-level keys and unknown
// service keys are ignored (compose files are often written for newer
// versions), but a service without image and without build, an invalid port,
// an invalid restart policy, an invalid memory/cpu value and a depends_on
// cycle are errors that name the offending service.
func Load(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read compose file: %w", err)
	}
	return Parse(data, filepath.Base(filepath.Dir(path)))
}

// Parse parses compose YAML from bytes. projectName is used as the project
// name when the file has no "name:" key.
func Parse(data []byte, projectName string) (*Project, error) {
	var f composeFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse compose file: %w", err)
	}
	p := &Project{Name: f.Name}
	if p.Name == "" {
		p.Name = projectName
	}
	names := make([]string, 0, len(f.Services))
	for name := range f.Services {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic service order
	for _, name := range names {
		node := f.Services[name]
		svc, err := parseService(name, &node)
		if err != nil {
			return nil, err
		}
		p.Services = append(p.Services, *svc)
	}
	if _, err := p.StartOrder(); err != nil {
		return nil, err
	}
	return p, nil
}

// StartOrder returns service names in an order that satisfies depends_on
// (dependencies first), and an error naming a cycle when one exists.
func (p *Project) StartOrder() ([]string, error) {
	depsOf := make(map[string][]string, len(p.Services))
	for i := range p.Services {
		deps := append([]string(nil), p.Services[i].DependsOn...)
		sort.Strings(deps)
		depsOf[p.Services[i].Name] = deps
	}
	const (
		unvisited = iota
		visiting
		visited
	)
	state := make(map[string]int, len(p.Services))
	var stack, order []string
	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case visited:
			return nil
		case visiting:
			start := 0
			for i, s := range stack {
				if s == name {
					start = i
					break
				}
			}
			cycle := append(append([]string(nil), stack[start:]...), name)
			return fmt.Errorf("dependency cycle: %s", strings.Join(cycle, " -> "))
		}
		state[name] = visiting
		stack = append(stack, name)
		for _, dep := range depsOf[name] {
			if _, ok := depsOf[dep]; !ok {
				return fmt.Errorf("service %q depends_on unknown service %q", name, dep)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[name] = visited
		order = append(order, name)
		return nil
	}
	roots := make([]string, 0, len(p.Services))
	for i := range p.Services {
		roots = append(roots, p.Services[i].Name)
	}
	sort.Strings(roots)
	for _, name := range roots {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// Service returns a service by name.
func (p *Project) Service(name string) (*Service, bool) {
	for i := range p.Services {
		if p.Services[i].Name == name {
			return &p.Services[i], true
		}
	}
	return nil, false
}

func parseService(name string, node *yaml.Node) (*Service, error) {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("service %q must be a mapping", name)
	}
	var raw rawService
	if err := node.Decode(&raw); err != nil {
		return nil, fmt.Errorf("service %q: %w", name, err)
	}
	svc := &Service{
		Name:        name,
		Image:       raw.Image,
		Restart:     raw.Restart,
		Dockerfile:  raw.Dockerfile,
		User:        raw.User,
		WorkingDir:  raw.WorkingDir,
		Hostname:    raw.Hostname,
		CapAdd:      raw.CapAdd,
		CapDrop:     raw.CapDrop,
		PidsLimit:   raw.PidsLimit,
		NetworkMode: raw.NetworkMode,
		Tty:         raw.Tty,
		Init:        raw.Init,
	}
	var err error
	if !isNull(&raw.Build) {
		context, dockerfile, buildErr := parseBuild(&raw.Build, name)
		if buildErr != nil {
			return nil, buildErr
		}
		svc.Build = context
		if dockerfile != "" {
			svc.Dockerfile = dockerfile
		}
	}
	if svc.Command, err = parseCommand(&raw.Command, "command", name); err != nil {
		return nil, err
	}
	if svc.Entrypoint, err = parseCommand(&raw.Entrypoint, "entrypoint", name); err != nil {
		return nil, err
	}
	if svc.Environment, err = parseEnvironment(&raw.Environment, name); err != nil {
		return nil, err
	}
	if svc.Ports, err = parseStringList(&raw.Ports, "ports", name); err != nil {
		return nil, err
	}
	if svc.Volumes, err = parseStringList(&raw.Volumes, "volumes", name); err != nil {
		return nil, err
	}
	if svc.DependsOn, err = parseDependsOn(&raw.DependsOn, name); err != nil {
		return nil, err
	}
	if svc.Labels, err = parseLabels(&raw.Labels, name); err != nil {
		return nil, err
	}
	if raw.HealthCheck != nil {
		hc, hcErr := parseHealthCheck(raw.HealthCheck, name)
		if hcErr != nil {
			return nil, hcErr
		}
		svc.HealthCheck = hc
	}
	if svc.Image == "" && svc.Build == "" {
		return nil, fmt.Errorf("service %q: either image or build is required", name)
	}
	for _, port := range svc.Ports {
		if _, err := network.ParsePortMapping(port); err != nil {
			return nil, fmt.Errorf("service %q: %w", name, err)
		}
	}
	if !validRestart(raw.Restart) {
		return nil, fmt.Errorf("service %q: invalid restart policy %q: use no, on-failure[:N], always or unless-stopped", name, raw.Restart)
	}
	mem, err := scalarValue(&raw.Memory, name, "mem_limit")
	if err != nil {
		return nil, err
	}
	if mem != "" {
		if _, err := cgroup.ParseBytes(mem); err != nil {
			return nil, fmt.Errorf("service %q: invalid mem_limit %q: %w", name, mem, err)
		}
		svc.Memory = mem
	}
	cpus, err := scalarValue(&raw.CPUs, name, "cpus")
	if err != nil {
		return nil, err
	}
	if cpus != "" {
		if _, err := cgroup.ParseCPUs(cpus); err != nil {
			return nil, fmt.Errorf("service %q: invalid cpus %q: %w", name, cpus, err)
		}
		svc.CPUs = cpus
	}
	return svc, nil
}

func parseBuild(node *yaml.Node, service string) (context, dockerfile string, err error) {
	if node.Kind == yaml.ScalarNode {
		return node.Value, "", nil
	}
	if node.Kind == yaml.MappingNode {
		var b struct {
			Context    string `yaml:"context"`
			Dockerfile string `yaml:"dockerfile"`
		}
		if err := node.Decode(&b); err != nil {
			return "", "", fmt.Errorf("service %q: invalid build: %w", service, err)
		}
		return b.Context, b.Dockerfile, nil
	}
	return "", "", fmt.Errorf("service %q: build must be a string or a mapping", service)
}

func parseCommand(node *yaml.Node, field, service string) ([]string, error) {
	if isNull(node) {
		return nil, nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		parts, err := splitCommand(node.Value)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", service, err)
		}
		return parts, nil
	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			s, err := scalarValue(item, service, field)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("service %q: %s must be a string or a list", service, field)
	}
}

func parseEnvironment(node *yaml.Node, service string) ([]string, error) {
	if isNull(node) {
		return nil, nil
	}
	values := map[string]string{}
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, err := scalarValue(node.Content[i], service, "environment name")
			if err != nil {
				return nil, err
			}
			value, err := scalarValue(node.Content[i+1], service, "environment value")
			if err != nil {
				return nil, err
			}
			values[key] = value
		}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			entry, err := scalarValue(item, service, "environment entry")
			if err != nil {
				return nil, err
			}
			key, value, found := strings.Cut(entry, "=")
			if !found {
				// Bare KEY would pass through the host env in compose;
				// minibox does not inherit it, so the value is empty.
				value = ""
			}
			values[key] = value
		}
	default:
		return nil, fmt.Errorf("service %q: environment must be a mapping or a list of KEY=value entries", service)
	}
	out := make([]string, 0, len(values))
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out, nil
}

func parseStringList(node *yaml.Node, field, service string) ([]string, error) {
	if isNull(node) {
		return nil, nil
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("service %q: %s must be a list", service, field)
	}
	out := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		s, err := scalarValue(item, service, field)
		if err != nil {
			return nil, fmt.Errorf("service %q: %s entries must be strings or numbers", service, field)
		}
		out = append(out, s)
	}
	return out, nil
}

func parseDependsOn(node *yaml.Node, service string) ([]string, error) {
	if isNull(node) {
		return nil, nil
	}
	switch node.Kind {
	case yaml.SequenceNode:
		out := make([]string, 0, len(node.Content))
		for _, item := range node.Content {
			s, err := scalarValue(item, service, "depends_on")
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	case yaml.MappingNode:
		// Keys are the dependencies; values like condition are ignored.
		out := make([]string, 0, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			out = append(out, node.Content[i].Value)
		}
		sort.Strings(out)
		return out, nil
	default:
		return nil, fmt.Errorf("service %q: depends_on must be a list or a mapping", service)
	}
}

func parseLabels(node *yaml.Node, service string) (map[string]string, error) {
	if isNull(node) {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("service %q: labels must be a mapping", service)
	}
	labels := make(map[string]string, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, err := scalarValue(node.Content[i], service, "label name")
		if err != nil {
			return nil, err
		}
		value, err := scalarValue(node.Content[i+1], service, "label value")
		if err != nil {
			return nil, err
		}
		labels[key] = value
	}
	return labels, nil
}

func parseHealthCheck(raw *rawHealthCheck, service string) (*HealthCheck, error) {
	hc := &HealthCheck{Retries: raw.Retries}
	if !isNull(&raw.Test) {
		switch raw.Test.Kind {
		case yaml.ScalarNode:
			hc.Test = []string{"CMD-SHELL", raw.Test.Value}
		case yaml.SequenceNode:
			hc.Test = make([]string, 0, len(raw.Test.Content))
			for _, item := range raw.Test.Content {
				s, err := scalarValue(item, service, "healthcheck test")
				if err != nil {
					return nil, err
				}
				hc.Test = append(hc.Test, s)
			}
		default:
			return nil, fmt.Errorf("service %q: healthcheck test must be a string or a list", service)
		}
	}
	var err error
	if hc.Interval, err = parseDuration(&raw.Interval, "healthcheck interval", service); err != nil {
		return nil, err
	}
	if hc.Timeout, err = parseDuration(&raw.Timeout, "healthcheck timeout", service); err != nil {
		return nil, err
	}
	if hc.StartPeriod, err = parseDuration(&raw.StartPeriod, "healthcheck start_period", service); err != nil {
		return nil, err
	}
	return hc, nil
}

func parseDuration(node *yaml.Node, field, service string) (time.Duration, error) {
	s, err := scalarValue(node, service, field)
	if err != nil || s == "" {
		return 0, err
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("service %q: invalid %s %q: use a duration like 30s, 1m30s or 500ms", service, field, s)
	}
	return d, nil
}

// scalarValue stringifies a scalar node: compose treats numbers and booleans
// in environment, labels and resource fields as strings.
func scalarValue(node *yaml.Node, service, field string) (string, error) {
	if isNull(node) {
		return "", nil
	}
	if node.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("service %q: %s must be a plain value", service, field)
	}
	return node.Value, nil
}

func isNull(node *yaml.Node) bool {
	return node == nil || node.Kind == 0 || (node.Kind == yaml.ScalarNode && node.Tag == "!!null")
}

func validRestart(policy string) bool {
	switch policy {
	case "", "no", "always", "unless-stopped", "on-failure":
		return true
	}
	if n, ok := strings.CutPrefix(policy, "on-failure:"); ok {
		count, err := strconv.Atoi(n)
		return err == nil && count >= 0
	}
	return false
}

// splitCommand splits a command string on whitespace, honouring single and
// double quotes like a shell would, without invoking one.
func splitCommand(s string) ([]string, error) {
	var out []string
	var b strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inWord {
				out = append(out, b.String())
				b.Reset()
				inWord = false
			}
		case c == '\'':
			inWord = true
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("unterminated single quote in command %q", s)
			}
			b.WriteString(s[i+1 : i+1+end])
			i += end + 1
		case c == '"':
			inWord = true
			closed := false
			for i++; i < len(s); i++ {
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					b.WriteByte(s[i+1])
					i++
					continue
				}
				if s[i] == '"' {
					closed = true
					break
				}
				b.WriteByte(s[i])
			}
			if !closed {
				return nil, fmt.Errorf("unterminated double quote in command %q", s)
			}
		default:
			inWord = true
			b.WriteByte(c)
		}
	}
	if inWord {
		out = append(out, b.String())
	}
	return out, nil
}
