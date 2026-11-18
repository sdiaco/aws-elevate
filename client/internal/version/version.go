// Package version holds the client version and the compatibility check between client
// and infrastructure.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// Client is this client's version, set at build time with -ldflags.
var Client = "0.0.2"

// MinInfra is the oldest infrastructure this client works with.
var MinInfra = "0.0.1"

// Compat is the outcome of Compare; the strings are those of contract/compat.json.
type Compat string

const (
	OK           Compat = "ok"
	ClientTooOld Compat = "client_too_old"
	InfraTooOld  Compat = "infra_too_old"
)

// Compare checks the client against the service minimum and the infrastructure against
// the client minimum. When both are too old the client wins: it is the one to upgrade
// first, since a newer client tells which infrastructure it needs.
func Compare(client, infra, minClient, minInfra string) (Compat, error) {
	c, err := Cmp(client, minClient)
	if err != nil {
		return "", err
	}
	if c < 0 {
		return ClientTooOld, nil
	}
	i, err := Cmp(infra, minInfra)
	if err != nil {
		return "", err
	}
	if i < 0 {
		return InfraTooOld, nil
	}
	return OK, nil
}

// Cmp compares two versions: -1, 0 or 1.
func Cmp(a, b string) (int, error) {
	x, err := parse(a)
	if err != nil {
		return 0, err
	}
	y, err := parse(b)
	if err != nil {
		return 0, err
	}
	for i := range x {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

func parse(v string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p != strconv.Itoa(n) {
			return out, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", v)
		}
		out[i] = n
	}
	return out, nil
}
