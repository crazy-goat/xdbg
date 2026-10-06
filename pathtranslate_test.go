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
	if got := fileURI("/var/www/proj/a.php"); got != "file:///var/www/proj/a.php" {
		t.Fatalf("fileURI = %q", got)
	}
}
