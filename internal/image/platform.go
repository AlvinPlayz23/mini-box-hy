package image

import (
	"fmt"
	"runtime"
	"strings"
)

// Platform identifies a target OS/architecture/variant.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

// CurrentPlatform reports the platform this binary is running on.
func CurrentPlatform() Platform {
	return Platform{OS: runtime.GOOS, Architecture: runtime.GOARCH}
}

// ParsePlatform parses "linux/amd64" or "linux/arm64/v8".
func ParsePlatform(s string) (Platform, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return Platform{}, fmt.Errorf("invalid platform %q", s)
	}
	p := Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	if p.OS == "" || p.Architecture == "" {
		return Platform{}, fmt.Errorf("invalid platform %q", s)
	}
	return p, nil
}

// String renders the platform.
func (p Platform) String() string {
	if p.Variant != "" {
		return p.OS + "/" + p.Architecture + "/" + p.Variant
	}
	return p.OS + "/" + p.Architecture
}

// Matches reports whether candidate p satisfies the wanted platform. An empty
// variant in the wanted platform matches any candidate variant.
func (p Platform) Matches(want Platform) bool {
	if want.OS != "" && want.OS != p.OS {
		return false
	}
	if want.Architecture != "" && want.Architecture != p.Architecture {
		return false
	}
	if want.Variant != "" && want.Variant != p.Variant {
		return false
	}
	return true
}
