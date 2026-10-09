package runtime

import "testing"

func TestParseSubIDFile(t *testing.T) {
	content := "# comment\nalice:100000:65536\nbob:200000:1\nmalformed\ncarol:x:1\ndave:300000:0\n"
	got, err := ParseSubIDFile(content, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.Start != 100000 || got.Count != 65536 {
		t.Errorf("alice range = %+v", got)
	}
	got, err = ParseSubIDFile(content, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if got.Start != 200000 || got.Count != 1 {
		t.Errorf("bob range = %+v", got)
	}
	if _, err := ParseSubIDFile(content, "carol"); err == nil {
		t.Error("carol has a non-numeric range but was accepted")
	}
	if _, err := ParseSubIDFile(content, "dave"); err == nil {
		t.Error("dave has a zero-length range but was accepted")
	}
	if _, err := ParseSubIDFile(content, "nobody"); err == nil {
		t.Error("unknown user was accepted")
	}
}
