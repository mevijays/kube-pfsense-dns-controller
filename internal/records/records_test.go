// SPDX-License-Identifier: Apache-2.0

package records

import (
	"reflect"
	"testing"
)

func TestSplit(t *testing.T) {
	d := ParseDomains(" mylab.lan, .Apps.MyLab.lan. ,")
	if !reflect.DeepEqual(d, Domains{"apps.mylab.lan", "mylab.lan"}) {
		t.Fatalf("ParseDomains = %v (longest domain must come first)", d)
	}
	cases := []struct {
		in, host, dom string
		ok            bool
	}{
		{"jenkins.mylab.lan", "jenkins", "mylab.lan", true},
		{"Jenkins.MyLab.LAN.", "jenkins", "mylab.lan", true},
		{"a.b.mylab.lan", "a.b", "mylab.lan", true},
		{"x.apps.mylab.lan", "x", "apps.mylab.lan", true}, // most specific domain wins
		{"mylab.lan", "", "", false},                      // bare domain
		{"*.mylab.lan", "", "", false},                    // wildcard
		{"jenkins.example.com", "", "", false},            // not managed
		{"notmylab.lan", "", "", false},                   // suffix must be a whole label
		{"", "", "", false},
	}
	for _, c := range cases {
		h, dm, ok := d.Split(c.in)
		if h != c.host || dm != c.dom || ok != c.ok {
			t.Errorf("Split(%q) = %q,%q,%v want %q,%q,%v", c.in, h, dm, ok, c.host, c.dom, c.ok)
		}
	}
}

func TestSetAddSortsAndDedupes(t *testing.T) {
	s := Set{}
	s.Add("app", "mylab.lan", "172.16.1.63", "HTTPRoute/b")
	s.Add("app", "mylab.lan", "172.16.1.61", "VirtualService/a")
	s.Add("app", "mylab.lan", "172.16.1.61", "VirtualService/a")
	s.Add("app", "mylab.lan", "not-an-ip", "VirtualService/a")
	r := s["app.mylab.lan"]
	if !reflect.DeepEqual(r.IPs, []string{"172.16.1.61", "172.16.1.63"}) {
		t.Errorf("IPs = %v", r.IPs)
	}
	if !reflect.DeepEqual(r.Sources, []string{"HTTPRoute/b", "VirtualService/a"}) {
		t.Errorf("Sources = %v", r.Sources)
	}
}
