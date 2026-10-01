package pytestcmd

import (
	"reflect"
	"testing"
)

func TestArgs(t *testing.T) {
	for _, tc := range []struct {
		command, want []string
	}{
		{[]string{"pytest", "-q"}, []string{"-q"}},
		{[]string{"/usr/bin/py.test"}, []string{}},
		{[]string{"python", "-m", "pytest", "tests"}, []string{"tests"}},
		{[]string{"/usr/local/bin/python3.12", "-m", "pytest"}, []string{}},
		{[]string{"python3", "pytest"}, nil},
		{[]string{"python3", "-m", "unittest"}, nil},
		{[]string{"python3.x", "-m", "pytest"}, nil},
		{[]string{"uv", "run", "pytest"}, nil},
		{[]string{"sh", "-c", "pytest"}, nil},
		{nil, nil},
	} {
		got := Args(tc.command)
		if (got == nil) != (tc.want == nil) || len(got) != len(tc.want) || len(got) > 0 && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Args(%q) = %q, want %q", tc.command, got, tc.want)
		}
	}
}
