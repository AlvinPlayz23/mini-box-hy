package build

import (
	"reflect"
	"strings"
	"testing"
)

const multiStageDockerfile = `# syntax=docker/dockerfile:1
# Build the minibox binary.
ARG GO_VERSION=1.23

FROM golang:$GO_VERSION AS builder
# Comments are ignored.
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod \
    go build \
    -o /out/minibox ./cmd/minibox

from alpine:3.20 as Runtime
RUN adduser -D app
COPY --from=0 --chmod=755 /out/minibox /usr/local/bin/minibox
USER app
ENTRYPOINT ["minibox"]
CMD ["--help"]
EXPOSE 2375
LABEL org.opencontainers.image.source="ghcr.io"
`

func TestParseDockerfileMultiStage(t *testing.T) {
	stages, err := ParseDockerfile(strings.NewReader(multiStageDockerfile))
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 2 {
		t.Fatalf("len(stages) = %d, want 2", len(stages))
	}

	s0 := stages[0]
	if s0.Index != 0 || s0.Base != "golang:$GO_VERSION" || s0.Name != "builder" {
		t.Errorf("stage 0 = %+v, want index 0, base golang:$GO_VERSION, name builder", s0)
	}
	if len(s0.Instructions) != 6 {
		t.Fatalf("stage 0 has %d instructions, want 6", len(s0.Instructions))
	}
	wantLines := []struct {
		cmd  string
		line int
	}{
		{"WORKDIR", 7}, {"COPY", 8}, {"RUN", 9}, {"COPY", 10}, {"COPY", 11}, {"RUN", 12},
	}
	for i, w := range wantLines {
		got := s0.Instructions[i]
		if got.Cmd != w.cmd || got.Line != w.line {
			t.Errorf("stage 0 instruction %d = %s line %d, want %s line %d", i, got.Cmd, got.Line, w.cmd, w.line)
		}
	}
	if args := s0.Instructions[0].Args; args != "/src" {
		t.Errorf("WORKDIR args = %q, want /src", args)
	}
	run := s0.Instructions[5]
	if run.Exec != nil {
		t.Errorf("shell-form RUN has Exec %v, want nil", run.Exec)
	}
	if got := run.Flags["mount"]; got != "type=cache,target=/go/pkg/mod" {
		t.Errorf("RUN Flags[mount] = %q, want type=cache,target=/go/pkg/mod", got)
	}
	if !strings.Contains(run.Args, "-o /out/minibox ./cmd/minibox") {
		t.Errorf("RUN args = %q, want the continued build command", run.Args)
	}

	s1 := stages[1]
	if s1.Index != 1 || s1.Base != "alpine:3.20" || s1.Name != "Runtime" {
		t.Errorf("stage 1 = %+v, want index 1, base alpine:3.20, name Runtime", s1)
	}
	if len(s1.Instructions) != 7 {
		t.Fatalf("stage 1 has %d instructions, want 7", len(s1.Instructions))
	}
	copyIn := s1.Instructions[1]
	if copyIn.Cmd != "COPY" || copyIn.Line != 18 || copyIn.Args != "/out/minibox /usr/local/bin/minibox" {
		t.Errorf("stage 1 instruction 1 = %+v, want COPY line 18", copyIn)
	}
	if copyIn.Flags["from"] != "0" || copyIn.Flags["chmod"] != "755" {
		t.Errorf("COPY flags = %v, want from=0 and chmod=755", copyIn.Flags)
	}
	if got := s1.Instructions[3]; got.Cmd != "ENTRYPOINT" || got.Line != 20 || !reflect.DeepEqual(got.Exec, []string{"minibox"}) {
		t.Errorf("stage 1 instruction 3 = %+v, want ENTRYPOINT line 20 exec [minibox]", got)
	}
	if got := s1.Instructions[4]; got.Cmd != "CMD" || !reflect.DeepEqual(got.Exec, []string{"--help"}) {
		t.Errorf("stage 1 instruction 4 = %+v, want CMD exec [--help]", got)
	}
	if got := s1.Instructions[5]; got.Cmd != "EXPOSE" || got.Args != "2375" || got.Exec != nil {
		t.Errorf("stage 1 instruction 5 = %+v, want shell-form EXPOSE 2375", got)
	}
	if got := s1.Instructions[6]; got.Cmd != "LABEL" || !strings.Contains(got.Args, "org.opencontainers.image.source=") {
		t.Errorf("stage 1 instruction 6 = %+v, want LABEL", got)
	}
}

func TestParseDockerfileErrors(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr []string
	}{
		{"unknown instruction", "FROM alpine\nMAINTAINER x\n", []string{"MAINTAINER", "line 2"}},
		{"unknown lowercase", "FROM alpine\nmaintainer x\n", []string{"MAINTAINER", "line 2"}},
		{"instruction before FROM", "RUN echo hi\n", []string{"RUN", "line 1"}},
		{"no FROM", "# only a comment\n", []string{"FROM"}},
		{"ARG only", "ARG X=1\n", []string{"FROM"}},
		{"unterminated JSON", "FROM alpine\nCMD [\"a\"\n", []string{"JSON", "line 2"}},
		{"invalid JSON array", "FROM alpine\nRUN [ -f /x ]\n", []string{"JSON", "line 2"}},
		{"non-string JSON", "FROM alpine\nCMD [\"a\", 1]\n", []string{"JSON", "line 2"}},
		{"text after JSON array", "FROM alpine\nCMD [\"a\"] extra\n", []string{"JSON", "line 2"}},
		{"FROM without base", "FROM\n", []string{"FROM"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDockerfile(strings.NewReader(tt.src))
			if err == nil {
				t.Fatalf("ParseDockerfile(%q) succeeded, want error", tt.src)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestParseDockerfileContinuations(t *testing.T) {
	src := "FROM alpine\n" +
		"RUN echo a \\\n" +
		"# a comment inside a continuation \\\n" +
		"    b\n"
	stages, err := ParseDockerfile(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	in := stages[0].Instructions[0]
	if got := strings.Join(strings.Fields(in.Args), " "); got != "echo a b" {
		t.Errorf("continued RUN args = %q, want %q", got, "echo a b")
	}

	src = "FROM alpine\n" +
		"RUN echo \"one \\\n" +
		"   two\" \\\n" +
		"   three\n"
	stages, err = ParseDockerfile(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	in = stages[0].Instructions[0]
	if got := strings.Join(strings.Fields(in.Args), " "); got != `echo "one two" three` {
		t.Errorf("quoted continuation args = %q, want %q", got, `echo "one two" three`)
	}

	stages, err = ParseDockerfile(strings.NewReader("FROM alpine\r\nRUN echo hi\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if args := stages[0].Instructions[0].Args; args != "echo hi" {
		t.Errorf("CRLF args = %q, want %q", args, "echo hi")
	}
}

func TestParseDockerfileExecForm(t *testing.T) {
	stages, err := ParseDockerfile(strings.NewReader("FROM alpine\nCMD [\"minibox\", \"--help\"]\nCMD []\n"))
	if err != nil {
		t.Fatal(err)
	}
	ins := stages[0].Instructions
	if !reflect.DeepEqual(ins[0].Exec, []string{"minibox", "--help"}) {
		t.Errorf("CMD exec = %v, want [minibox --help]", ins[0].Exec)
	}
	if ins[0].Args != `["minibox", "--help"]` {
		t.Errorf("CMD raw args = %q, want the JSON text", ins[0].Args)
	}
	if ins[1].Exec == nil || len(ins[1].Exec) != 0 {
		t.Errorf("empty JSON array exec = %v, want non-nil empty", ins[1].Exec)
	}

	stages, err = ParseDockerfile(strings.NewReader("FROM alpine\nADD app.tar.gz /\n"))
	if err != nil {
		t.Fatal(err)
	}
	add := stages[0].Instructions[0]
	if add.Cmd != "ADD" || add.Args != "app.tar.gz /" || add.Exec != nil {
		t.Errorf("ADD = %+v, want shell-form ADD app.tar.gz /", add)
	}
}

func TestVarsFromInstructions(t *testing.T) {
	src := `FROM alpine:3.20 AS base
ARG VER=1.0
ENV APP=demo VER=2.0
ARG VER=3.0

from scratch as final
ARG VER
COPY --from=0 /x /y
`
	stages, err := ParseDockerfile(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}

	vars := VarsFromInstructions(stages, nil)
	if vars["VER"] != "2.0" || vars["APP"] != "demo" {
		t.Errorf("vars = %v, want VER=2.0 APP=demo (env wins over arg)", vars)
	}

	vars = VarsFromInstructions(stages, map[string]string{"VER": "9.9"})
	if vars["VER"] != "2.0" {
		t.Errorf("vars = %v, want VER=2.0 (env wins over build args)", vars)
	}
}

func TestVarsFromInstructionsBuildArgs(t *testing.T) {
	src := "FROM alpine:3.20\nARG VER=1.0\nARG OTHER\n"
	stages, err := ParseDockerfile(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}

	vars := VarsFromInstructions(stages, map[string]string{"VER": "9.9", "OTHER": "x"})
	if vars["VER"] != "9.9" || vars["OTHER"] != "x" {
		t.Errorf("vars = %v, want VER=9.9 OTHER=x", vars)
	}

	vars = VarsFromInstructions(stages, nil)
	if vars["VER"] != "1.0" {
		t.Errorf("vars = %v, want VER=1.0", vars)
	}
	if _, ok := vars["OTHER"]; ok {
		t.Errorf("ARG without a default or build arg should stay unset, got %v", vars)
	}
}
