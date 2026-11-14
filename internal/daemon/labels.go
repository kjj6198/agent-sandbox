//go:build linux

package daemon

import (
	"regexp"
	"strings"
)

// Labels are a client's own metadata on a sandbox, such as which app created it, so it can
// find its sandboxes again after it restarts.
const (
	maxLabels     = 32
	maxLabelValue = 256
)

var labelKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)

func validLabels(labels map[string]string) error {
	if len(labels) > maxLabels {
		return invalid("a sandbox has at most %d labels", maxLabels)
	}
	for k, v := range labels {
		if !labelKeyRE.MatchString(k) {
			return invalid("label key %q must be 1-63 letters, digits, '.', '_', '/', or '-'", k)
		}
		if len(v) > maxLabelValue {
			return invalid("label %s is longer than %d bytes", k, maxLabelValue)
		}
	}
	return nil
}

// parseLabelSelector reads ?label=KEY=VALUE parameters. A sandbox matches when it has
// every one of them.
func parseLabelSelector(params []string) (map[string]string, error) {
	sel := map[string]string{}
	for _, p := range params {
		k, v, ok := strings.Cut(p, "=")
		if !ok || !labelKeyRE.MatchString(k) {
			return nil, invalid("label filter %q must be KEY=VALUE", p)
		}
		sel[k] = v
	}
	return sel, nil
}

func matchLabels(labels, sel map[string]string) bool {
	for k, v := range sel {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}
