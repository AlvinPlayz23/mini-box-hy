//go:build linux

package runtime

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// UserSpec is a parsed --user value: either numeric ids or names to look up.
type UserSpec struct {
	UID, GID       int
	User, Group    string
	HasUID, HasGID bool
}

// ParseUserSpec parses "uid", "uid:gid", "user", "user:group". Empty input
// means "no override" and returns a zero UserSpec with HasUID false.
func ParseUserSpec(s string) (UserSpec, error) {
	var u UserSpec
	s = strings.TrimSpace(s)
	if s == "" {
		return u, nil
	}
	parts := strings.SplitN(s, ":", 2)
	if n, err := strconv.Atoi(parts[0]); err == nil {
		u.UID, u.HasUID = n, true
	} else {
		if parts[0] == "" {
			return u, fmt.Errorf("invalid user %q", s)
		}
		u.User = parts[0]
	}
	if len(parts) == 2 {
		if parts[1] == "" {
			return u, fmt.Errorf("invalid group in %q", s)
		}
		if n, err := strconv.Atoi(parts[1]); err == nil {
			u.GID, u.HasGID = n, true
		} else {
			u.Group = parts[1]
		}
	}
	return u, nil
}

// Lookup resolves names against the passwd/group files of a container root.
func (u *UserSpec) Lookup(root string) error {
	if u.User != "" {
		uid, gid, err := lookupPasswd(filepath.Join(root, "etc/passwd"), u.User)
		if err != nil {
			return err
		}
		u.UID, u.HasUID = uid, true
		if !u.HasGID {
			u.GID, u.HasGID = gid, true
		}
	}
	if u.Group != "" {
		gid, err := lookupGroup(filepath.Join(root, "etc/group"), u.Group)
		if err != nil {
			return err
		}
		u.GID, u.HasGID = gid, true
	}
	return nil
}

// ParsePasswdLine parses one /etc/passwd line into uid and gid.
func ParsePasswdLine(line string) (name string, uid, gid int, ok bool) {
	fields := strings.Split(line, ":")
	if len(fields) < 4 {
		return "", 0, 0, false
	}
	u, err1 := strconv.Atoi(fields[2])
	g, err2 := strconv.Atoi(fields[3])
	if err1 != nil || err2 != nil {
		return "", 0, 0, false
	}
	return fields[0], u, g, true
}

// ParseGroupLine parses one /etc/group line into a gid.
func ParseGroupLine(line string) (name string, gid int, ok bool) {
	fields := strings.Split(line, ":")
	if len(fields) < 3 {
		return "", 0, false
	}
	g, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, false
	}
	return fields[0], g, true
}

func lookupPasswd(path, name string) (uid, gid int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("resolve user %q: %w", name, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n, u, g, ok := ParsePasswdLine(sc.Text())
		if ok && n == name {
			return u, g, nil
		}
	}
	return 0, 0, fmt.Errorf("user %q not found in %s", name, path)
}

func lookupGroup(path, name string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("resolve group %q: %w", name, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n, g, ok := ParseGroupLine(sc.Text())
		if ok && n == name {
			return g, nil
		}
	}
	return 0, fmt.Errorf("group %q not found in %s", name, path)
}
