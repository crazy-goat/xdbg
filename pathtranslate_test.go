package main

import "testing"

// newSession trims trailing slashes, so a configured root is always prefix-safe.
func TestNewSessionTrimsTrailingSlashes(t *testing.T) {
	s := newSession("/host/proj/", "/var/www/proj/")
	if s.localRoot != "/host/proj" || s.dockerRoot != "/var/www/proj" {
		t.Fatalf("roots = %q / %q, want them without a trailing slash", s.localRoot, s.dockerRoot)
	}
}

func TestToContainer(t *testing.T) {
	s := newSession("/host/proj", "/var/www/proj")
	for name, tc := range map[string]struct{ in, want string }{
		"host absolute":          {"/host/proj/src/Foo.php", "/var/www/proj/src/Foo.php"},
		"project relative":       {"src/Foo.php", "/var/www/proj/src/Foo.php"},
		"other absolute path":    {"/src/Foo.php", "/src/Foo.php"}, // absolute, not under localRoot: pass through
		"exact local root":       {"/host/proj", "/var/www/proj"},
		"already container path": {"/var/www/proj/src/Foo.php", "/var/www/proj/src/Foo.php"},
		"other absolute file":    {"/etc/php.ini", "/etc/php.ini"},
		"nested relative":        {"src/Foo/Bar.php", "/var/www/proj/src/Foo/Bar.php"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := s.toContainer(tc.in); got != tc.want {
				t.Fatalf("toContainer(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestToHost(t *testing.T) {
	s := newSession("/host/proj", "/var/www/proj")
	for name, tc := range map[string]struct{ in, want string }{
		"fileuri":              {"file:///var/www/proj/src/Foo.php", "/host/proj/src/Foo.php"},
		"plain container path": {"/var/www/proj/src/Foo.php", "/host/proj/src/Foo.php"},
		"exact docker root":    {"file:///var/www/proj", "/host/proj"},
		"outside docker root":  {"file:///other/abs.php", "/other/abs.php"},
		"empty":                {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := s.toHost(tc.in); got != tc.want {
				t.Fatalf("toHost(%q) = %q, want %q", tc.in, got, tc.want)
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
