// Package build implements a Dockerfile subset for image builds: instruction
// parsing, argument expansion, cache keys and build-context COPY resolution.
package build

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Instruction is one Dockerfile instruction.
type Instruction struct {
	Cmd   string            // uppercase: FROM, RUN, COPY, ENV, WORKDIR, CMD, ENTRYPOINT, LABEL, EXPOSE, USER, ARG
	Args  string            // raw argument text after the command and flags
	Flags map[string]string // --flag=value pairs (for example COPY --from=0, RUN --mount=type=cache)
	Exec  []string          // non-nil when the argument is a JSON array (exec form)
	Line  int               // 1-based line number of the instruction
}

// Stage is one FROM..next FROM block. A Dockerfile with no FROM is an error.
type Stage struct {
	Index        int
	Base         string // image reference from FROM
	Name         string // stage name from "FROM x AS name"
	Instructions []Instruction
}

// knownCmds is the supported instruction set. ADD is accepted because it
// parses (and caches) exactly like COPY.
var knownCmds = map[string]bool{
	"FROM": true, "RUN": true, "COPY": true, "ADD": true, "ENV": true,
	"WORKDIR": true, "CMD": true, "ENTRYPOINT": true, "LABEL": true,
	"EXPOSE": true, "USER": true, "ARG": true,
}

type rawLine struct {
	text string
	line int
}

// ParseDockerfile parses a Dockerfile, handling comments, blank lines, line
// continuations, exec-form JSON arrays, and \ escapes inside shell words.
func ParseDockerfile(r io.Reader) ([]Stage, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read dockerfile: %w", err)
	}
	var stages []Stage
	for _, l := range logicalLines(string(data)) {
		in, err := parseInstruction(l.text, l.line)
		if err != nil {
			return nil, err
		}
		if in.Cmd == "FROM" {
			base, name, err := parseFrom(in.Args, in.Line)
			if err != nil {
				return nil, err
			}
			stages = append(stages, Stage{Index: len(stages), Base: base, Name: name})
			continue
		}
		if len(stages) == 0 {
			if in.Cmd != "ARG" {
				return nil, fmt.Errorf("line %d: %s before the first FROM", in.Line, in.Cmd)
			}
			continue
		}
		last := &stages[len(stages)-1]
		last.Instructions = append(last.Instructions, in)
	}
	if len(stages) == 0 {
		return nil, errors.New("dockerfile contains no FROM instruction")
	}
	return stages, nil
}

// logicalLines joins physical lines on backslash-newline continuations and
// drops blank lines and comments, including comments inside continuations.
func logicalLines(src string) []rawLine {
	var out []rawLine
	var b strings.Builder
	start, active := 0, false
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !active {
			start = i + 1
			b.Reset()
			b.WriteString(trimmed)
		} else {
			b.WriteString(strings.TrimRight(line, " \t\r"))
		}
		body := strings.TrimRight(b.String(), " \t\r")
		if strings.HasSuffix(body, "\\") {
			b.Reset()
			b.WriteString(body[:len(body)-1])
			active = true
			continue
		}
		out = append(out, rawLine{text: b.String(), line: start})
		active = false
	}
	if active {
		out = append(out, rawLine{text: b.String(), line: start})
	}
	return out
}

// parseInstruction parses one logical line into an Instruction.
func parseInstruction(text string, line int) (Instruction, error) {
	s := strings.TrimSpace(text)
	end := 0
	for end < len(s) && s[end] != ' ' && s[end] != '\t' {
		end++
	}
	word := strings.ToUpper(s[:end])
	if !knownCmds[word] {
		return Instruction{}, fmt.Errorf("line %d: unknown instruction %q", line, word)
	}
	rest := strings.TrimLeft(s[end:], " \t")
	flags := map[string]string{}
	for strings.HasPrefix(rest, "--") && rest != "--" {
		tok, tail := rest, ""
		if j := strings.IndexAny(rest, " \t"); j >= 0 {
			tok, tail = rest[:j], strings.TrimLeft(rest[j:], " \t")
		}
		name, value := tok[2:], ""
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name, value = name[:eq], name[eq+1:]
		}
		if name == "" {
			break
		}
		flags[name] = value
		rest = tail
	}
	in := Instruction{Cmd: word, Args: rest, Flags: flags, Line: line}
	if strings.HasPrefix(in.Args, "[") {
		exec, err := parseExecForm(in.Args)
		if err != nil {
			return Instruction{}, fmt.Errorf("line %d: %v", line, err)
		}
		in.Exec = exec
	}
	return in, nil
}

// parseExecForm parses a JSON-array exec form such as ["minibox","--help"].
func parseExecForm(args string) ([]string, error) {
	dec := json.NewDecoder(strings.NewReader(args))
	var exec []string
	if err := dec.Decode(&exec); err != nil {
		return nil, fmt.Errorf("invalid JSON array %q: %v", args, err)
	}
	if tail := strings.TrimSpace(args[dec.InputOffset():]); tail != "" {
		return nil, fmt.Errorf("unexpected text %q after JSON array", tail)
	}
	if exec == nil {
		exec = []string{}
	}
	return exec, nil
}

// parseFrom extracts the base image and optional stage name from FROM args.
func parseFrom(args string, line int) (base, name string, err error) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "", "", fmt.Errorf("line %d: FROM requires a base image", line)
	}
	base = fields[0]
	for i := 1; i+1 < len(fields); i++ {
		if strings.EqualFold(fields[i], "as") {
			name = fields[i+1]
			break
		}
	}
	return base, name, nil
}
