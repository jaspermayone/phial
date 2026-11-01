package extract

import (
	"context"
	"fmt"

	"github.com/jaspermayone/phial/internal/store"
	"github.com/jaspermayone/phial/internal/tenant"
)

// TriageResult counts what Triage did with the pending proposals.
type TriageResult struct {
	Pending   int
	Kept      int // still a proposal: restates the phial and changes it
	Confirmed int // rejected, and the target confirmed: it said the same
	New       int // rejected, and remembered as a new phial: a sibling fact
	Skipped   int // no target text to compare against
}

// Triage re-sorts the pending proposals with Classify, for proposals made
// before the coverage rule existed. Without apply it only counts.
func Triage(ctx context.Context, v *tenant.View, apply bool, log func(string, ...any)) (TriageResult, error) {
	var res TriageResult
	ps, err := v.Proposals(ctx, "pending", 10000)
	if err != nil {
		return res, err
	}
	res.Pending = len(ps)
	for _, p := range ps {
		if p.Target == nil || p.Target.Text == "" {
			res.Skipped++
			continue
		}
		cov, cont := store.Compare(p.Target.Text, p.Text)
		class := Classify(cov, cont)
		log("%s %-7s cov=%.2f cont=%.2f %s: %.80s", p.ID, class, cov, cont, p.Subject, p.Text)
		switch class {
		case "propose":
			res.Kept++
			continue
		case "confirm":
			res.Confirmed++
			if !apply {
				continue
			}
			if _, err := v.Resolve(ctx, p.ID, false); err != nil {
				return res, fmt.Errorf("reject %s: %w", p.ID, err)
			}
			if _, err := v.ConfirmAs(ctx, p.TargetID, Actor); err != nil {
				return res, fmt.Errorf("confirm %s: %w", p.TargetID, err)
			}
		default:
			res.New++
			if !apply {
				continue
			}
			if _, err := v.Resolve(ctx, p.ID, false); err != nil {
				return res, fmt.Errorf("reject %s: %w", p.ID, err)
			}
			if _, _, err := v.Remember(ctx, store.RememberInput{
				Subject: p.Subject, Kind: p.Kind, Text: p.Text, Paths: p.Paths,
				Origin: "extracted", Actor: Actor,
				Provenance: store.Provenance{SessionID: p.SessionID, Source: "triage of " + p.ID},
			}); err != nil {
				return res, fmt.Errorf("remember for %s: %w", p.ID, err)
			}
		}
	}
	return res, nil
}
