// SPDX-License-Identifier: Apache-2.0

package records

import (
	"strings"
	"testing"
)

// FuzzSplit checks that Split never panics and that anything it accepts is
// reassembled into the normalised input.
func FuzzSplit(f *testing.F) {
	for _, s := range []string{"a.mylab.lan", "A.B.MYLAB.LAN.", "*.mylab.lan", "mylab.lan", ".mylab.lan", "..", "", "xn--bcher-kva.mylab.lan", "a b.mylab.lan", "mylab.lan.mylab.lan"} {
		f.Add(s)
	}
	d := ParseDomains("mylab.lan,apps.mylab.lan")
	f.Fuzz(func(t *testing.T, in string) {
		host, dom, ok := d.Split(in)
		if !ok {
			if host != "" || dom != "" {
				t.Fatalf("not ok but returned %q %q", host, dom)
			}
			return
		}
		if host == "" || dom == "" || strings.Contains(host, "*") {
			t.Fatalf("accepted bad split of %q: %q %q", in, host, dom)
		}
		norm := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in), "."))
		if host+"."+dom != norm {
			t.Fatalf("round trip %q -> %q.%q", in, host, dom)
		}
	})
}
