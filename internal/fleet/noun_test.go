package fleet

import (
	"testing"

	"github.com/goptics/vartop/internal/config"
)

// The instance noun every server has, and a "pod" one that fixtures use to
// check that pages word themselves by the Noun they are given.
var (
	podNoun      = Noun{One: "pod", Many: "pods", Article: "a", Param: "pod"}
	instanceNoun = Noun{One: "instance", Many: "instances", Article: "an", Param: "instance"}
)

func TestNounFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  *config.Server
		want Noun
	}{
		{"static endpoints", &config.Server{Endpoints: []string{"http://a:1", "http://b:1"}}, instanceNoun},
		{"dns", &config.Server{DNS: &config.DNSRef{Host: "varmq-headless", Port: 8080}}, instanceNoun},
		{"nil", nil, instanceNoun},
	} {
		if got := NounFor(tc.srv); got != tc.want {
			t.Errorf("%s: NounFor = %+v, want %+v", tc.name, got, tc.want)
		}
	}

	for _, tc := range []struct {
		n         Noun
		cap, many string
	}{
		{podNoun, "Pod", "Pods"},
		{instanceNoun, "Instance", "Instances"},
		{Noun{}, "", ""}, // the zero Noun a bare Page carries must not panic
	} {
		if got := tc.n.Cap(); got != tc.cap {
			t.Errorf("%+v.Cap() = %q, want %q", tc.n, got, tc.cap)
		}
		if got := tc.n.CapMany(); got != tc.many {
			t.Errorf("%+v.CapMany() = %q, want %q", tc.n, got, tc.many)
		}
	}
}
