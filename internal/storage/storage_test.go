package storage

import "testing"

func TestCleanRel(t *testing.T) {
	ok := map[string]string{"": "", "a/b": "a/b", "/a//b/": "a/b", `a\b`: "a/b", "./x": "x"}
	for in, want := range ok {
		got, err := CleanRel(in)
		if err != nil || got != want {
			t.Errorf("%q -> %q, %v", in, got, err)
		}
	}
	// traversal is neutralized by Clean ("/../x" -> "x"); never escapes root
	if got, _ := CleanRel("../../etc/passwd"); got != "etc/passwd" {
		t.Errorf("traversal: %q", got)
	}
}

func TestSecrets(t *testing.T) {
	existing := map[string]string{"secret_access_key": "S3CRET", "bucket": "b"}
	in := map[string]string{"secret_access_key": SecretMask, "bucket": "b2"}
	out := MergeSecrets("s3", in, existing)
	if out["secret_access_key"] != "S3CRET" || out["bucket"] != "b2" {
		t.Fatal(out)
	}
	m := Masked("s3", existing)
	if m["secret_access_key"] != SecretMask || m["bucket"] != "b" {
		t.Fatal(m)
	}
}

func TestNormalizeToken(t *testing.T) {
	pasted := "Paste the following into your remote machine --->\n{\"access_token\":\"a\",\n \"token_type\":\"Bearer\",\"refresh_token\":\"r\",\"expiry\":\"2026-01-01T00:00:00Z\"}\n<---End paste"
	if got := NormalizeToken(pasted); got != `{"access_token":"a","token_type":"Bearer","refresh_token":"r","expiry":"2026-01-01T00:00:00Z"}` {
		t.Fatal(got)
	}
}

func TestParseCallback(t *testing.T) {
	code, state, err := ParseCallback("http://127.0.0.1:53682/?state=abc&code=4%2F0Ad")
	if err != nil || code != "4/0Ad" || state != "abc" {
		t.Fatal(code, state, err)
	}
	if _, _, err := ParseCallback("http://localhost:53682/?error=access_denied"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRemoteAbsolutePath(t *testing.T) {
	abs := &rcloneBackend{root: "/backup/mysql", st: nil}
	if got := abs.remote("job-1/20261018-full"); got != "dumpcat:/backup/mysql/job-1/20261018-full" {
		t.Fatal(got)
	}
	rel := &rcloneBackend{root: "bucket/prefix"}
	if got := rel.remote("x"); got != "dumpcat:bucket/prefix/x" {
		t.Fatal(got)
	}
	if got := rel.remote(""); got != "dumpcat:bucket/prefix" {
		t.Fatal(got)
	}
}
