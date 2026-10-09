package build

import "strings"

// VarsFromInstructions collects ARG and ENV values in order, so later
// instructions can be expanded. env wins over arg for the same key.
func VarsFromInstructions(stages []Stage, buildArgs map[string]string) map[string]string {
	vars := map[string]string{}
	envSet := map[string]bool{}
	for _, st := range stages {
		for _, in := range st.Instructions {
			switch in.Cmd {
			case "ARG":
				key, def, hasDefault := splitArgKey(in.Args)
				if key == "" || envSet[key] {
					continue
				}
				if v, ok := buildArgs[key]; ok {
					vars[key] = v
				} else if hasDefault {
					vars[key] = def
				}
			case "ENV":
				for _, kv := range parseEnvPairs(in.Args) {
					if kv[0] == "" {
						continue
					}
					vars[kv[0]] = kv[1]
					envSet[kv[0]] = true
				}
			}
		}
	}
	return vars
}

// splitArgKey parses "KEY" or "KEY=default" from an ARG instruction.
func splitArgKey(args string) (key, value string, hasDefault bool) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return "", "", false
	}
	f := fields[0]
	if eq := strings.IndexByte(f, '='); eq >= 0 {
		return f[:eq], f[eq+1:], true
	}
	return f, "", false
}

// parseEnvPairs parses "KEY=value" pairs from an ENV instruction. Words
// without "=" extend the previous value, and the legacy "ENV key value" form
// is accepted when no "=" is present at all.
func parseEnvPairs(args string) [][2]string {
	words := strings.Fields(args)
	if len(words) == 0 {
		return nil
	}
	if !strings.Contains(args, "=") {
		return [][2]string{{words[0], strings.TrimSpace(args[len(words[0]):])}}
	}
	var out [][2]string
	for _, w := range words {
		if eq := strings.IndexByte(w, '='); eq >= 0 {
			out = append(out, [2]string{w[:eq], w[eq+1:]})
		} else if len(out) > 0 {
			out[len(out)-1][1] += " " + w
		}
	}
	return out
}
