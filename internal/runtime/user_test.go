package runtime

import "testing"

func TestParseUserSpec(t *testing.T) {
	tests := []struct {
		in      string
		uid     int
		gid     int
		user    string
		group   string
		hasUID  bool
		hasGID  bool
		wantErr bool
	}{
		{in: "", hasUID: false},
		{in: "1000", uid: 1000, hasUID: true},
		{in: "1000:1000", uid: 1000, gid: 1000, hasUID: true, hasGID: true},
		{in: "nobody", user: "nobody"},
		{in: "nobody:nogroup", user: "nobody", group: "nogroup"},
		{in: "0:0", uid: 0, gid: 0, hasUID: true, hasGID: true},
		{in: ":", wantErr: true},
		{in: "user:", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseUserSpec(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseUserSpec(%q) = %+v, want error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseUserSpec(%q): %v", tt.in, err)
			continue
		}
		if got.UID != tt.uid || got.GID != tt.gid || got.User != tt.user || got.Group != tt.group ||
			got.HasUID != tt.hasUID || got.HasGID != tt.hasGID {
			t.Errorf("ParseUserSpec(%q) = %+v", tt.in, got)
		}
	}
}

func TestParsePasswdLine(t *testing.T) {
	name, uid, gid, ok := ParsePasswdLine("alice:x:1000:1000:Alice:/home/alice:/bin/sh")
	if !ok || name != "alice" || uid != 1000 || gid != 1000 {
		t.Errorf("got %q %d %d %v", name, uid, gid, ok)
	}
	if _, _, _, ok := ParsePasswdLine("malformed"); ok {
		t.Error("malformed line parsed")
	}
	if _, _, _, ok := ParsePasswdLine("a:x:notanumber:1"); ok {
		t.Error("non-numeric uid parsed")
	}
}

func TestParseGroupLine(t *testing.T) {
	name, gid, ok := ParseGroupLine("staff:x:50:")
	if !ok || name != "staff" || gid != 50 {
		t.Errorf("got %q %d %v", name, gid, ok)
	}
	if _, _, ok := ParseGroupLine("bad"); ok {
		t.Error("malformed group line parsed")
	}
}
