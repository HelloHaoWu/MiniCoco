package main

import "context"

// Read-only selection: never acquire the precompact lease or wait for its worker.
// Raw ordinals alone are insufficient after a formal summary changes the view.
func findReadySummary(records []Summary, raw, current []Message, end, count int, config string) *Summary {
	consumed := map[string]bool{}
	for _, s := range records {
		if !s.Ready && s.DerivedFrom != "" && validateSummary(s, raw) == nil {
			consumed[s.DerivedFrom] = true
		}
	}
	var best *Summary
	for i := range records {
		s := &records[i]
		if !s.Ready || consumed[s.UsageID] || s.ConfigHash != config || s.PrefixCount > count || validateSummary(*s, raw) != nil {
			continue
		}
		viewEnd := end - (count - s.PrefixCount)
		if viewEnd < 3 || viewEnd > end || s.SourceHash != historyDigest(current[:viewEnd]) {
			continue
		}
		if best == nil || s.PrefixCount > best.PrefixCount {
			best = s
		}
	}
	return best
}

func checkedSummaryView(current []Message, end int, content string) ([]Message, error) {
	next := append(summaryMessages(content), current[end:]...)
	if len(next) >= len(current) || estimateTokens(next) >= estimateTokens(current) {
		return nil, cannotCompress("summary did not reduce context")
	}
	if err := historyBudget(next); err != nil {
		return nil, cannotCompress(err.Error())
	}
	return next, nil
}

// Append a formal checkpoint instead of overwriting the candidate. DerivedFrom
// records consumption durably; the original paid computation remains recoverable.
func (s *sessionService) commitReusedSummary(ctx context.Context, sid string, ready, checkpoint Summary, rawCount int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.meta.CommitRoundUsage(ctx, sid, ready.UsageID, ready.Usage, rawCount); err != nil {
		return storageFailure(err)
	}
	if err := s.transcript.SaveSummary(ctx, sid, checkpoint); err != nil {
		return storageFailure(err)
	}
	if err := s.meta.CommitRoundUsage(context.WithoutCancel(ctx), sid, checkpoint.UsageID, checkpoint.Usage, rawCount); err != nil {
		return storageFailure(err)
	}
	return nil
}
