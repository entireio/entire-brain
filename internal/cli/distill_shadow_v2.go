package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	distillCandidatePromptVersionV2       = "candidate-extraction-v2"
	distillCandidateOllamaPromptVersionV2 = "candidate-extraction-v3-strict-negative"
	distillCandidateRedactionVersionV2    = "redaction-v1"

	// The local entire-brain distillation model defaults to num_predict=512.
	// That is sufficient for a single legacy chunk but can cut off a valid
	// candidate-ID-framed pack before every member has a completion. The budget
	// scales with the number of independently attributable members and remains
	// bounded below the local model's context window.
	distillCandidateOllamaNumPredictFloorV2     = 1024
	distillCandidateOllamaNumPredictPerMemberV2 = 256
	distillCandidateOllamaNumPredictCeilingV2   = 8192
)

// distillCandidateShadowMemberV2 joins one redacted provider member to the
// extraction identity and session ownership that intentionally stay outside
// the provider payload. Materialization, when requested, is a separate step.
type distillCandidateShadowMemberV2 struct {
	PackMember      distillCandidatePackMemberV2
	SourceSessionID string
	Identity        distillCandidateCacheIdentityV2
}

type distillCandidatePackAttemptV2 struct {
	Results   []distillCandidateMemberResultV2
	Err       error
	holdsSlot bool
}

type distillCandidatePackPrefetchV2 struct {
	Results  []chan distillCandidatePackAttemptV2
	Release  func(distillCandidatePackAttemptV2)
	Cancel   context.CancelFunc
	Wait     func()
	Launched *atomic.Int64
}

// runDistillCandidateExtractionV2 is the shared Phase 2/3 extraction path. It
// may update only the v2 member-result cache; callers decide whether the
// protocol-valid results are merely observed or materialized afterward.
func runDistillCandidateExtractionV2(
	ctx context.Context,
	repoDir, brainDir string,
	args []string,
	prompt string,
	taxonomy factTaxonomy,
	sessions []exportSession,
	snapshots map[string]candidateDistillInputSnapshot,
	opts distillCommandOptions,
	resolveBranch func(exportSession) string,
	usageCollector *distillUsageCollector,
	runStarted time.Time,
	now time.Time,
) (*factSourceManifest, error) {
	cache, cacheErr := loadDistillCandidateResultCacheV2(brainDir)
	var warnings []string
	if cacheErr != nil {
		// The v2 cache is disposable derived state. Reconstruct it from successful
		// framed results instead of letting corruption affect active Brain state.
		cache = newDistillCandidateResultCacheV2()
		warnings = append(warnings, "candidate result cache was unreadable and will be rebuilt from protocol-valid results")
	}

	baseIdentity := distillCandidateCacheBaseIdentityV2(prompt, taxonomy, args, opts)
	ordered := make([]distillCandidateShadowMemberV2, 0)
	byCandidate := make(map[string]distillCandidateShadowMemberV2)
	selectedSessions := 0
	admittedCards := 0
	var candidateBytes int64
	for _, session := range sessions {
		if !distillSessionSelectedForOptionsV2(session, resolveBranch(session), opts) {
			continue
		}
		selectedSessions++
		snapshot, ok := snapshots[distillCandidateSnapshotKey(session, resolveBranch(session))]
		if !ok {
			return nil, fmt.Errorf("candidate shadow preflight snapshot missing for %s", filepath.ToSlash(session.TranscriptPath))
		}
		for _, batch := range snapshot.input.CandidateBatches {
			admittedCards++
			if len(batch.CandidateIDs) != 1 || len(batch.Anchors) != 1 || batch.CandidateIDs[0] != batch.Anchors[0].CandidateID {
				return nil, fmt.Errorf("candidate shadow found an invalid Phase 1 card boundary for %s", filepath.ToSlash(session.TranscriptPath))
			}
			member := distillCandidatePackMemberV2{
				CandidateID:  batch.CandidateIDs[0],
				RenderedCard: batch.Chunk.Text,
				Anchor:       batch.Anchors[0],
			}
			candidateBytes += int64(len(member.RenderedCard))
			shadowMember := distillCandidateShadowMemberV2{
				PackMember:      member,
				SourceSessionID: strings.TrimSpace(batch.Anchors[0].SessionID),
				Identity:        baseIdentity.withCard(member.RenderedCard),
			}
			if shadowMember.SourceSessionID == "" {
				return nil, fmt.Errorf("candidate shadow member %q has no source session ID", member.CandidateID)
			}
			if prior, duplicate := byCandidate[member.CandidateID]; duplicate {
				if prior.PackMember.RenderedCard != member.RenderedCard || prior.Identity != shadowMember.Identity || prior.SourceSessionID != shadowMember.SourceSessionID {
					return nil, fmt.Errorf("candidate shadow member %q has conflicting content-addressed views", member.CandidateID)
				}
				continue // branch replay / duplicate export: extract once
			}
			byCandidate[member.CandidateID] = shadowMember
			ordered = append(ordered, shadowMember)
		}
	}

	misses := make([]distillCandidatePackMemberV2, 0, len(ordered))
	cacheHits := 0
	shadowFacts := 0
	emptyResults := 0
	for _, member := range ordered {
		if !opts.force {
			if cached, ok := cache.LookupSuccess(member.PackMember.CandidateID, member.Identity); ok {
				cacheHits++
				shadowFacts += len(cached.Facts)
				if cached.Empty {
					emptyResults++
				}
				continue
			}
		}
		misses = append(misses, member.PackMember)
	}
	packs, err := packDistillCandidateMembersForOptionsV2(misses, opts)
	if err != nil {
		return nil, err
	}

	cacheDirty := false
	callsSinceFlush := 0
	flushInterval := opts.flushEvery
	if flushInterval <= 0 {
		flushInterval = distillFlushCallInterval
	}
	flushCache := func(force bool) error {
		if !cacheDirty || (!force && callsSinceFlush < flushInterval) {
			return nil
		}
		if err := withBrainWriteLock(brainDir, func() error {
			return saveDistillCandidateResultCacheV2(brainDir, cache)
		}); err != nil {
			return err
		}
		cacheDirty = false
		callsSinceFlush = 0
		return nil
	}

	storeResults := func(results []distillCandidateMemberResultV2) error {
		for _, result := range results {
			member, ok := byCandidate[result.CandidateID]
			if !ok {
				return fmt.Errorf("candidate shadow result references unknown member %q", result.CandidateID)
			}
			cached, err := canonicalDistillCandidateCacheResultV2(result, taxonomy)
			if err != nil {
				return err
			}
			if err := cache.PutSuccess(result.CandidateID, member.SourceSessionID, member.Identity, cached); err != nil {
				return err
			}
			cacheDirty = true
			shadowFacts += len(cached.Facts)
			if cached.Empty {
				emptyResults++
			}
		}
		return nil
	}

	providerStarted := time.Now()
	prefetch := startDistillCandidatePackPrefetchV2(ctx, repoDir, args, packs, taxonomy, opts)
	defer func() {
		prefetch.Cancel()
		prefetch.Wait()
	}()
	packCompleted := 0
	failedCalls := 0
	splitCalls := 0
	unresolvedMembers := 0
	anyProviderSuccess := false
	for index, pack := range packs {
		var attempt distillCandidatePackAttemptV2
		select {
		case attempt = <-prefetch.Results[index]:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// A failed parent retains its semaphore slot while its two children run
		// serially. The slot is a lease for the whole isolation attempt, so split
		// retries can never raise provider concurrency above --concurrency.
		if err := func() error {
			defer prefetch.Release(attempt)
			callsSinceFlush++
			if attempt.Err == nil {
				if err := storeResults(attempt.Results); err != nil {
					return err
				}
				anyProviderSuccess = true
				packCompleted++
				return flushCache(false)
			}

			failedCalls++
			warnings = append(warnings, fmt.Sprintf("candidate pack %d failed and was isolated before materialization", index+1))
			if len(pack.Members) == 1 {
				unresolvedMembers++
				if !anyProviderSuccess && failedCalls >= distillAgentAbortThreshold {
					_ = flushCache(true)
					return fmt.Errorf("candidate shadow aborted after %d provider/protocol failures with no valid response", failedCalls)
				}
				return nil
			}

			left, right := bisectDistillCandidatePackV2(pack)
			children := []distillCandidatePackV2{left, right}
			resolved := true
			for childIndex, child := range children {
				splitCalls++
				callsSinceFlush++
				childAttempt := runDistillCandidatePackAttemptV2(ctx, repoDir, args, child, taxonomy, opts)
				if childAttempt.Err != nil {
					failedCalls++
					resolved = false
					unresolvedMembers += len(child.Members)
					warnings = append(warnings, fmt.Sprintf("candidate pack %d split %d failed; its members remain uncached", index+1, childIndex+1))
					if !anyProviderSuccess && failedCalls >= distillAgentAbortThreshold {
						_ = flushCache(true)
						return fmt.Errorf("candidate shadow aborted after %d provider/protocol failures with no valid response", failedCalls)
					}
					continue
				}
				if err := storeResults(childAttempt.Results); err != nil {
					return err
				}
				anyProviderSuccess = true
				if err := flushCache(false); err != nil {
					return err
				}
			}
			if resolved {
				packCompleted++
			}
			return nil
		}(); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := flushCache(true); err != nil {
		return nil, err
	}
	if !opts.shadow && unresolvedMembers > 0 {
		return nil, fmt.Errorf("candidate extraction left %d member(s) unresolved; protocol-valid neighbors were cached but active facts were not changed", unresolvedMembers)
	}

	totalCalls := int(prefetch.Launched.Load()) + splitCalls
	source := &factSourceManifest{
		GeneratedAt:                now,
		TaxonomyPath:               factsTaxonomyPath,
		ChunksScanned:              len(packs),
		ChunksDistilled:            packCompleted,
		CacheHits:                  cacheHits,
		FailedChunks:               failedCalls,
		CandidateBytes:             candidateBytes,
		CandidateCards:             admittedCards,
		CandidateSessions:          selectedSessions,
		CandidatePacks:             len(packs),
		CandidatePacksDone:         packCompleted,
		CandidateMembers:           len(ordered),
		CandidateUnresolvedMembers: unresolvedMembers,
		CandidateCacheHits:         cacheHits,
		CandidateCacheMisses:       len(misses),
		CandidateSplitCalls:        splitCalls,
		CandidateEmptyResults:      emptyResults,
		Agent:                      strings.TrimSpace(opts.agent),
		Model:                      strings.TrimSpace(opts.model),
		Effort:                     strings.TrimSpace(opts.effort),
		Pipeline:                   distillPipelineCandidates,
		Branch:                     strings.TrimSpace(opts.branch),
		Force:                      opts.force,
		Jobs:                       distillRequestedJobs(opts),
		ExtractionJobsCap:          distillEffectiveExtractionJobs(len(packs), opts),
		MaxChunkBytes:              distillCandidatePackMaxRenderedBytesV2,
		ExtractionCalls:            totalCalls,
		TotalAgentCalls:            totalCalls,
		TokenUsage:                 usageCollector.summary(totalCalls),
		ExtractionWaitSeconds:      time.Since(providerStarted).Seconds(),
		TotalSeconds:               time.Since(runStarted).Seconds(),
		Warnings:                   capWarnings(warnings, maxDistillWarnings),
		Shadow:                     opts.shadow,
	}
	if opts.shadow {
		source.ShadowFacts = shadowFacts
	}
	return source, nil
}

func distillSessionSelectedForOptionsV2(session exportSession, branch string, opts distillCommandOptions) bool {
	if opts.branch != "" && branch != opts.branch {
		return false
	}
	return opts.session == "" || strings.TrimSpace(session.SessionID) == strings.TrimSpace(opts.session)
}

type distillCandidateCacheBaseIdentityV2Value struct {
	PromptVersion      string
	PromptDigest       string
	TaxonomyDigest     string
	AgentCommandDigest string
	Model              string
	Effort             string
}

func distillCandidateCacheBaseIdentityV2(prompt string, taxonomy factTaxonomy, args []string, opts distillCommandOptions) distillCandidateCacheBaseIdentityV2Value {
	model := strings.TrimSpace(opts.model)
	if model == "" {
		model = "(default)"
	}
	effort := strings.TrimSpace(opts.effort)
	if effort == "" {
		effort = "(default)"
	}
	promptVersion := distillCandidatePromptVersionV2
	if len(args) > 0 && args[0] == "ollama" {
		promptVersion = distillCandidateOllamaPromptVersionV2
	}
	return distillCandidateCacheBaseIdentityV2Value{
		PromptVersion:      promptVersion,
		PromptDigest:       distillCandidateCacheDigestV2(prompt),
		TaxonomyDigest:     distillCandidateCacheDigestV2(factTaxonomyBlock(taxonomy)),
		AgentCommandDigest: distillCandidateCacheDigestV2(strings.Join(args, "\x00")),
		Model:              model,
		Effort:             effort,
	}
}

func (base distillCandidateCacheBaseIdentityV2Value) withCard(renderedCard string) distillCandidateCacheIdentityV2 {
	return distillCandidateCacheIdentityV2{
		CardDigest:         distillCandidateCacheDigestV2(renderedCard),
		PromptVersion:      base.PromptVersion,
		PromptDigest:       base.PromptDigest,
		TaxonomyDigest:     base.TaxonomyDigest,
		AgentCommandDigest: base.AgentCommandDigest,
		Model:              base.Model,
		Effort:             base.Effort,
		RedactionVersion:   distillCandidateRedactionVersionV2,
	}
}

func canonicalDistillCandidateCacheResultV2(result distillCandidateMemberResultV2, taxonomy factTaxonomy) (distillCandidateCacheResultV2, error) {
	if result.NoFacts {
		return distillCandidateCacheResultV2{Empty: true}, nil
	}
	out := distillCandidateCacheResultV2{}
	seen := make(map[string]struct{}, len(result.Facts))
	for _, fact := range result.Facts {
		paths := normalizeFactPaths([]string{fact.Path})
		if len(paths) != 1 || !factPathKnownTopLevel(taxonomy, paths[0]) {
			return distillCandidateCacheResultV2{}, fmt.Errorf("candidate %q produced a fact outside the active taxonomy", result.CandidateID)
		}
		text := truncateString(strings.TrimSpace(redactText(fact.Text)), distillFactMaxTextSize)
		if text == "" {
			return distillCandidateCacheResultV2{}, fmt.Errorf("candidate %q produced an empty fact", result.CandidateID)
		}
		key := fact.Kind + "\x00" + paths[0] + "\x00" + text
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out.Facts = append(out.Facts, distillCandidateCachedFactV2{Kind: fact.Kind, Paths: paths, Text: text})
	}
	if len(out.Facts) == 0 {
		return distillCandidateCacheResultV2{}, fmt.Errorf("candidate %q produced no canonical facts", result.CandidateID)
	}
	return out, nil
}

func runDistillCandidatePackAttemptV2(ctx context.Context, repoDir string, args []string, pack distillCandidatePackV2, taxonomy factTaxonomy, opts distillCommandOptions) (attempt distillCandidatePackAttemptV2) {
	defer func() {
		if recovered := recover(); recovered != nil {
			attempt = distillCandidatePackAttemptV2{Err: fmt.Errorf("candidate shadow provider panicked")}
		}
	}()
	ctx = withDistillDecodedOutputLimit(ctx, distillCandidateMaxOutputBytesV2)
	if len(args) > 0 && args[0] == "ollama" {
		ctx = withDistillOllamaNumPredict(ctx, distillCandidateOllamaNumPredictV2(len(pack.Members)))
	}
	out, err := opts.run(ctx, repoDir, args, []byte(pack.ProviderInput), opts.timeout)
	if err != nil {
		return distillCandidatePackAttemptV2{Err: err}
	}
	expected := make([]string, len(pack.Members))
	for index := range pack.Members {
		expected[index] = pack.Members[index].CandidateID
	}
	var results []distillCandidateMemberResultV2
	if len(args) > 0 && args[0] == "ollama" {
		results, err = parseDistillCandidateMemberResultsOllamaV2(expected, out)
	} else {
		results, err = parseDistillCandidateMemberResultsV2(expected, out)
	}
	if err != nil {
		return distillCandidatePackAttemptV2{Err: err}
	}
	// Taxonomy/redaction validation is part of the atomic pack protocol. A
	// syntactically framed response with one semantically invalid member must be
	// isolated before any neighbor is cached.
	for _, result := range results {
		if _, err := canonicalDistillCandidateCacheResultV2(result, taxonomy); err != nil {
			return distillCandidatePackAttemptV2{Err: err}
		}
	}
	return distillCandidatePackAttemptV2{Results: results}
}

func distillCandidateOllamaNumPredictV2(members int) int {
	if members < 1 {
		return distillCandidateOllamaNumPredictFloorV2
	}
	numPredict := members * distillCandidateOllamaNumPredictPerMemberV2
	if numPredict < distillCandidateOllamaNumPredictFloorV2 {
		return distillCandidateOllamaNumPredictFloorV2
	}
	if numPredict > distillCandidateOllamaNumPredictCeilingV2 {
		return distillCandidateOllamaNumPredictCeilingV2
	}
	return numPredict
}

func startDistillCandidatePackPrefetchV2(ctx context.Context, repoDir string, args []string, packs []distillCandidatePackV2, taxonomy factTaxonomy, opts distillCommandOptions) distillCandidatePackPrefetchV2 {
	runCtx, cancel := context.WithCancel(ctx)
	results := make([]chan distillCandidatePackAttemptV2, len(packs))
	for index := range results {
		results[index] = make(chan distillCandidatePackAttemptV2, 1)
	}
	jobs := distillRequestedJobs(opts)
	if jobs < 1 {
		jobs = 1
	}
	sem := make(chan struct{}, jobs)
	var calls sync.WaitGroup
	var dispatcher sync.WaitGroup
	launched := &atomic.Int64{}
	dispatcher.Add(1)
	go func() {
		defer dispatcher.Done()
		for index := range packs {
			select {
			case sem <- struct{}{}:
			case <-runCtx.Done():
				return
			}
			launched.Add(1)
			calls.Add(1)
			go func(index int) {
				defer calls.Done()
				attempt := runDistillCandidatePackAttemptV2(runCtx, repoDir, args, packs[index], taxonomy, opts)
				attempt.holdsSlot = true
				results[index] <- attempt
			}(index)
		}
	}()
	return distillCandidatePackPrefetchV2{
		Results: results,
		Release: func(attempt distillCandidatePackAttemptV2) {
			if attempt.holdsSlot {
				<-sem
			}
		},
		Cancel: cancel,
		Wait: func() {
			dispatcher.Wait()
			calls.Wait()
		},
		Launched: launched,
	}
}

func bisectDistillCandidatePackV2(pack distillCandidatePackV2) (distillCandidatePackV2, distillCandidatePackV2) {
	middle := len(pack.Members) / 2
	return distillCandidatePackFromMembersV2(pack.Members[:middle]), distillCandidatePackFromMembersV2(pack.Members[middle:])
}

func distillCandidatePackFromMembersV2(members []distillCandidatePackMemberV2) distillCandidatePackV2 {
	pack := distillCandidatePackV2{Members: append([]distillCandidatePackMemberV2(nil), members...)}
	var input strings.Builder
	for _, member := range members {
		input.WriteString(member.RenderedCard)
	}
	pack.ProviderInput = input.String()
	return pack
}
