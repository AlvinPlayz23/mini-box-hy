package image

import "testing"

func TestParsePlatform(t *testing.T) {
	p, err := ParsePlatform("linux/arm64/v8")
	if err != nil {
		t.Fatal(err)
	}
	if p.OS != "linux" || p.Architecture != "arm64" || p.Variant != "v8" {
		t.Errorf("got %+v", p)
	}
	if got := p.String(); got != "linux/arm64/v8" {
		t.Errorf("String() = %q", got)
	}
	for _, bad := range []string{"", "linux", "linux/amd64/v1/extra", "/amd64"} {
		if _, err := ParsePlatform(bad); err == nil {
			t.Errorf("ParsePlatform(%q) succeeded, want error", bad)
		}
	}
}

func TestPlatformMatches(t *testing.T) {
	cur := CurrentPlatform()
	if !cur.Matches(cur) {
		t.Error("platform should match itself")
	}
	if cur.Matches(Platform{OS: "windows", Architecture: cur.Architecture}) {
		t.Error("linux should not match windows")
	}
	arm := Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	if !arm.Matches(Platform{OS: "linux", Architecture: "arm64"}) {
		t.Error("empty variant should match any variant")
	}
	if arm.Matches(Platform{OS: "linux", Architecture: "arm64", Variant: "v7"}) {
		t.Error("v7 should not match v8")
	}
}
