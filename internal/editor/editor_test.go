package editor

import (
	"slices"
	"testing"
)

func TestResolve(t *testing.T) {
	if o := Resolve("ZED"); o.Command != "zed" || o.ArgsTemplate != "%file:%line" {
		t.Fatalf("zed: %+v", o)
	}
	for _, id := range []string{"", "vim", "atom"} {
		if o := Resolve(id); o.ID != DefaultID {
			t.Errorf("%q resolved to %s", id, o.ID)
		}
	}
	for _, id := range []string{NoneID, CustomID} {
		if o := Resolve(id); o.Command != "" || o.ArgsTemplate != "" {
			t.Errorf("%s: %+v", id, o)
		}
	}
}

func TestParseArgsTemplate(t *testing.T) {
	cases := map[string][]string{
		"-l %line %file":               {"-l", "%line", "%file"},
		"  -l   %line\t%file  ":        {"-l", "%line", "%file"},
		`+%line "%file"`:               {"+%line", "%file"},
		`'%file' -line %line`:          {"%file", "-line", "%line"},
		`"%file":%line`:                {"%file:%line"},
		"":                             {},
		"   \t  ":                      {},
		`--msg "say \"hi\""`:           {"--msg", `say "hi"`},
		`"a\\b"`:                       {`a\b`},
		`"\s+ \$PATH"`:                 {`\s+ \$PATH`},
		`'say \"hi\"'`:                 {`say \"hi\"`},
		`--eval "(f \"%file\" %line)"`: {"--eval", `(f "%file" %line)`},
	}
	for in, want := range cases {
		if got := ParseArgsTemplate(in); !slices.Equal(got, want) {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestBuildArgs(t *testing.T) {
	if got := BuildArgs(`-l %line "%file"`, "/tmp/file.ts", 12); !slices.Equal(got, []string{"-l", "12", "/tmp/file.ts"}) {
		t.Fatalf("got %q", got)
	}
	if got := BuildArgs("%file:%line", "/a b/c", 0); !slices.Equal(got, []string{"/a b/c:1"}) {
		t.Fatalf("got %q", got)
	}
}
