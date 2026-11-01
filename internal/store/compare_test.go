package store

import "testing"

func TestCompare(t *testing.T) {
	target := "MentorDb production runs on mrm-prod (NixOS VM, Tailscale 100.95.157.35), reached via ssh -J alastor jsp@mrm-prod.wildebeest-stargazer.ts.net. Config lives in jaspermayone/dots under hosts/mrm-prod."
	cases := []struct {
		name           string
		cand           string
		minCov, maxCov float64
		minCont        float64
	}{
		{"same text", target, 0.99, 1, 0.99},
		{"restates and adds", target + " The staging VM is mrm-staging on the same tailnet.", 0.99, 1, 0.5},
		{"sibling fact", "Staging for MentorDB is the tailnet host mrm-staging.wildebeest-stargazer.ts.net, reached via ssh -J alastor.", 0, 0.5, 0},
		{"unrelated", "COMP2100 coursework lives in the private repo jaspermayone/wit-f26-comp2100.", 0, 0.1, 0},
	}
	for _, c := range cases {
		cov, cont := Compare(target, c.cand)
		if cov < c.minCov || cov > c.maxCov || cont < c.minCont {
			t.Errorf("%s: coverage=%.2f containment=%.2f, want coverage in [%.2f,%.2f] and containment >= %.2f", c.name, cov, cont, c.minCov, c.maxCov, c.minCont)
		}
	}
	if cov, cont := Compare("", "x"); cov != 0 || cont != 0 {
		t.Errorf("empty target: %v %v", cov, cont)
	}
}
