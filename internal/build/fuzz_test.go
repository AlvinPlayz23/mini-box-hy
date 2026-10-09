package build

import (
	"strings"
	"testing"
)

func FuzzParseDockerfile(f *testing.F) {
	f.Add("# syntax=docker/dockerfile:1\nFROM alpine:3.20\nRUN [\"echo\", \"hi\"]\n")
	f.Add("FROM golang:1.23 AS b\nCOPY --from=0 [\"a\", \"b\"] /out\n")
	f.Add("RUN echo \\\n\thi \\\n# comment \\\n\tworld\n")
	f.Add("CMD [\"unterminated\n")
	f.Add("$$$$${VAR:-${NESTED\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, src string) {
		stages, err := ParseDockerfile(strings.NewReader(src))
		if err == nil && len(stages) == 0 {
			t.Error("ParseDockerfile returned no stages and no error")
		}
	})
}
