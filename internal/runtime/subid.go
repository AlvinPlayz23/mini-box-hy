//go:build linux

package runtime

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// SubIDRange is a range from /etc/subuid or /etc/subgid.
type SubIDRange struct {
	Start int
	Count int
}

// ParseSubIDFile finds the range assigned to name in a subuid/subgid file.
func ParseSubIDFile(content, name string) (SubIDRange, error) {
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 3 || fields[0] != name {
			continue
		}
		start, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || start < 0 || count <= 0 {
			continue
		}
		return SubIDRange{Start: start, Count: count}, nil
	}
	return SubIDRange{}, fmt.Errorf("no subid range for %q", name)
}

// SubIDAvailable reports whether newuidmap/newgidmap and the subid files allow
// a multi-id mapping for the current user.
func SubIDAvailable() (SubIDRange, SubIDRange, bool) {
	if _, err := exec.LookPath("newuidmap"); err != nil {
		return SubIDRange{}, SubIDRange{}, false
	}
	if _, err := exec.LookPath("newgidmap"); err != nil {
		return SubIDRange{}, SubIDRange{}, false
	}
	name := os.Getenv("USER")
	if name == "" {
		name = strconv.Itoa(os.Geteuid())
	}
	uidContent, err := os.ReadFile("/etc/subuid")
	if err != nil {
		return SubIDRange{}, SubIDRange{}, false
	}
	gidContent, err := os.ReadFile("/etc/subgid")
	if err != nil {
		return SubIDRange{}, SubIDRange{}, false
	}
	uids, err := ParseSubIDFile(string(uidContent), name)
	if err != nil {
		if uids, err = ParseSubIDFile(string(uidContent), strconv.Itoa(os.Geteuid())); err != nil {
			return SubIDRange{}, SubIDRange{}, false
		}
	}
	gids, err := ParseSubIDFile(string(gidContent), name)
	if err != nil {
		if gids, err = ParseSubIDFile(string(gidContent), strconv.Itoa(os.Getegid())); err != nil {
			return SubIDRange{}, SubIDRange{}, false
		}
	}
	return uids, gids, true
}

// WriteMappings runs newuidmap/newgidmap for a child process so that container
// ids map onto the caller's subid ranges.
func WriteMappings(pid int, uids, gids SubIDRange) error {
	uidArgs := []string{strconv.Itoa(pid), "0", strconv.Itoa(os.Geteuid()), "1"}
	gidArgs := []string{strconv.Itoa(pid), "0", strconv.Itoa(os.Getegid()), "1"}
	if uids.Count > 0 {
		uidArgs = append(uidArgs, "1", strconv.Itoa(uids.Start), strconv.Itoa(uids.Count))
	}
	if gids.Count > 0 {
		gidArgs = append(gidArgs, "1", strconv.Itoa(gids.Start), strconv.Itoa(gids.Count))
	}
	if out, err := exec.Command("newuidmap", uidArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("newuidmap failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("newgidmap", gidArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("newgidmap failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
