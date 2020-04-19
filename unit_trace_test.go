package mqtt

import (
	"fmt"
	"strings"
	"testing"

	. "github.com/eclipse/paho.mqtt.golang"
)

type testLogger struct {
	out []string
}

func (t *testLogger) Println(v ...interface{}) {
	s := []string{}
	for _, val := range v {
		s = append(s, fmt.Sprintf("%v", val))
	}
	t.out = append(t.out, strings.Join(s, " "))
}

func (t *testLogger) Printf(f string, v ...interface{}) {
	t.out = append(t.out, fmt.Sprintf(f, v...))
}

func Test_OverrideNOOPLogger(t *testing.T) {
	l := &testLogger{}
	ERROR = l
	ERRORD.Dumpln(Milieu{}, "hello", "world")
	if len(l.out) != 1 {
		if l.out[1] != "hello world" {
			t.Fatalf("unexpected output")
		} else {
			t.Fatalf("output empty")
		}
	}
}
