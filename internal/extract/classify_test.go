package extract

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		cov, cont float64
		want      string
	}{
		{1, 1, "confirm"},       // identical
		{0.95, 0.92, "confirm"}, // reworded, same content
		{1, 0.5, "propose"},     // restates the target and adds detail
		{0.75, 0.6, "propose"},  // mostly the target, some change
		{0.6, 0.9, "new"},       // a short sibling fact made of the target's words
		{0.3, 0.4, "new"},       // shares a few terms only
		{0, 0, "new"},           // nothing near
	}
	for _, c := range cases {
		if got := Classify(c.cov, c.cont); got != c.want {
			t.Errorf("Classify(%.2f, %.2f) = %s, want %s", c.cov, c.cont, got, c.want)
		}
	}
}
