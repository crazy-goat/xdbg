package main

import "testing"

func TestNewSessionCleansRoots(t *testing.T) {
	for name, tc := range map[string]struct{ local, docker, wantLocal, wantDocker string }{
		"trailing slashes":  {"/host/proj/", "/var/www/proj/", "/host/proj", "/var/www/proj"},
		"dot segments":      {"/host//work/../proj/.", "/var//www/app/../proj/.", "/host/proj", "/var/www/proj"},
		"filesystem roots":  {"/", "/", "/", "/"},
		"empty docker root": {"/host//proj/.", "", "/host/proj", ""},
	} {
		t.Run(name, func(t *testing.T) {
			s := newSession(tc.local, tc.docker)
			if s.localRoot != tc.wantLocal || s.dockerRoot != tc.wantDocker {
				t.Fatalf("roots = %q / %q, want %q / %q", s.localRoot, s.dockerRoot, tc.wantLocal, tc.wantDocker)
			}
		})
	}
}

func TestPathTranslationRoundTrip(t *testing.T) {
	s := newSession("/host/proj", "/var/www/proj")
	host := "/host/proj/src/Foo/Bar.php"
	if got := s.toHost("file://" + s.toContainer(host)); got != host {
		t.Fatalf("round trip = %q, want %q", got, host)
	}
}

func TestFileURI(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain path", "/app/a.php", "file:///app/a.php"},
		{"spaces", "/app/my dir/a b.php", "file:///app/my%20dir/a%20b.php"},
		{"percent", "/app/100%.php", "file:///app/100%25.php"},
		{"literal percent escape", "/app/literal%20.php", "file:///app/literal%2520.php"},
		{"fragment and query", "/app/a#b?c.php", "file:///app/a%23b%3Fc.php"},
		{"quote and backslash", `/app/a"b\c.php`, "file:///app/a%22b%5Cc.php"},
		{"non-ASCII", "/app/żółć.php", "file:///app/%C5%BC%C3%B3%C5%82%C4%87.php"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fileURI(tc.in); got != tc.want {
				t.Fatalf("fileURI(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}
