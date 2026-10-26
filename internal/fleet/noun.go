package fleet

import (
	"strings"

	"github.com/goptics/vartop/internal/config"
)

// Noun is what the pages call one unit of a server: "pod" on Kubernetes,
// "instance" on every other server. Article goes before One ("a pod", "an
// instance"). Param is the URL parameter that names a unit by its label.
type Noun struct{ One, Many, Article, Param string }

// Cap is One with a capital first letter.
func (n Noun) Cap() string { return capitalize(n.One) }

// CapMany is Many with a capital first letter.
func (n Noun) CapMany() string { return capitalize(n.Many) }

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// NounFor is the noun of srv's units: a Kubernetes server's are pods, any
// other server's (static endpoints, DNS) are instances, and so are nil's.
func NounFor(srv *config.Server) Noun {
	if srv != nil && srv.Kubernetes != nil {
		return Noun{One: "pod", Many: "pods", Article: "a", Param: "pod"}
	}
	return Noun{One: "instance", Many: "instances", Article: "an", Param: "instance"}
}
