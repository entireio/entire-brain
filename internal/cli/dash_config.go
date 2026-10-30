package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/entire-brain/internal/cli/command/dash"
	clibase "github.com/entireio/entire-brain/internal/cli/internal/base"
	bmanifest "github.com/entireio/entire-brain/internal/cli/internal/manifest"
	"github.com/entireio/entire-brain/internal/tui"
)

func dashConfig(opts Options) dash.Config {
	return dash.Config{Options: opts, Deps: dashDeps()}
}

// dashDeps binds the cli-private seams the dash command needs; the transport
// and tombstone tests that drive dash.Run or dash.AssembleBrainSnapshot
// directly reuse it.
func dashDeps() dash.Deps {
	return dash.Deps{
		AgentSurfaceTarget:    agentSurfaceTarget,
		ResolveFactsTarget:    resolveFactsTarget,
		StatusHome:            dashStatusHome,
		RetrieveUnified:       retrieveUnified,
		BufferRetrievalOutput: bufferRetrievalCommandOutput,
		TerminalWriter:        dashTerminalWriter,
		BrainSourceReader:     brainSourceReader,
	}
}

// dashStatusHome binds the cli-private status report to the dashboard: it runs
// the full report, loads the session read guard, and maps the report into the
// Home tab view, handing over only importable values.
func dashStatusHome(ctx context.Context, opts Options, target, branch, brainDir string) (dash.StatusHome, error) {
	status, err := buildBrainStatusReport(ctx, opts, target)
	if err != nil {
		return dash.StatusHome{}, err
	}
	manifest := status.Manifest
	guard, err := bmanifest.LoadSessionReadGuard(brainDir, manifest)
	if err != nil {
		return dash.StatusHome{}, err
	}
	return dash.StatusHome{
		Home:        dashHomeView(status, manifest, branch, guard),
		Manifest:    manifest,
		Guard:       guard,
		RepoRoot:    status.Repo.Root,
		GeneratedAt: status.Brain.GeneratedAt,
	}, nil
}

// dashTerminalWriter is the stream the dashboard program renders to: every
// write still goes through the privacy re-check, but the terminal underneath
// stays visible to the terminal library.
//
// That second half is not a nicety. bubbletea learns the window size by calling
// Fd() on its output and only if the output satisfies term.File (an
// io.ReadWriteCloser with an Fd); anything else is "can't query window size",
// so it never sends the initial WindowSizeMsg and never installs the SIGWINCH
// resize watcher. A plain privacyLinearizedWriter is exactly that anything
// else — one Write method — so the dashboard sat on its pre-size placeholder
// ("loading…") forever on a real terminal, and resizing it did nothing. It
// still accepted keys and still quit on q, which is why exit-code checks never
// caught it.
//
// Embedding the file carries Read/Close/Fd through untouched while Write stays
// on the audited path, so the privacy boundary is unchanged.
func dashTerminalWriter(out io.Writer, policy retrievalPrivacyPolicy) io.Writer {
	guarded := privacyLinearizedWriter{out: out, policies: []retrievalPrivacyPolicy{policy}}
	file, ok := out.(*os.File)
	if !ok {
		return guarded
	}
	return dashTTYWriter{File: file, guarded: guarded}
}

// dashTTYWriter is a terminal that writes through the privacy guard. The
// embedded *os.File supplies Read/Close/Fd (what bubbletea probes for a window
// size); Write is overridden so no byte reaches the terminal unchecked.
type dashTTYWriter struct {
	*os.File
	guarded privacyLinearizedWriter
}

func (w dashTTYWriter) Write(p []byte) (int, error) { return w.guarded.Write(p) }

// dashHomeView maps the status report into the Home tab view model.
func dashHomeView(status brainStatusReport, manifest *exportManifest, branch string, guards ...sessionReadGuard) tui.HomeView {
	home := tui.HomeView{
		Freshness:  brainStatusFreshnessSeverity(status),
		Warnings:   append([]string(nil), status.Warnings...),
		BlindSpots: dashBlindSpotLabels(brainStatusBlindSpots(status)),
	}

	present := status.Sources
	detail := dashSourceDetails(manifest, guards...)
	home.Sources = []tui.SourceHealth{
		{Name: "Seed", Present: present.Seed, Detail: detail["seed"]},
		{Name: "Sessions", Present: present.Sessions, Detail: detail["sessions"]},
		{Name: "Code", Present: present.Semantic, Detail: detail["semantic"]},
		{Name: "History", Present: present.History, Detail: detail["history"]},
		{Name: "Facts", Present: present.Facts, Detail: detail["facts"]},
	}

	if status.Semantic != nil && status.Semantic.Freshness != nil {
		for name, axis := range status.Semantic.Freshness.Axes {
			home.Axes = append(home.Axes, tui.FreshnessAxis{Name: name, State: axis.State, Detail: axis.Detail})
		}
		sortFreshnessAxes(home.Axes)
	}

	home.Live = tui.LiveState{
		Branch:  clibase.FirstNonEmpty(status.Live.Branch, branch),
		Head:    status.Live.Head,
		Dirty:   status.Live.Dirty,
		Summary: dashLiveSummary(status.Live),
	}

	if manifest != nil && manifest.GeneratedAt.IsZero() && !anySource(present) {
		home.Warnings = append(home.Warnings, "brain not built yet — run `"+clibase.BrainInvocation(os.LookupEnv, "refresh")+"`")
	}
	return home
}

func dashSourceDetails(manifest *exportManifest, guards ...sessionReadGuard) map[string]string {
	out := map[string]string{}
	if manifest == nil || manifest.Sources == nil {
		return out
	}
	src := manifest.Sources
	if s := src.Sessions; s != nil {
		count, checkpoints := len(s.Sessions), s.CheckpointsScanned
		if len(guards) > 0 && !guards[0].Empty() {
			visible := bmanifest.GuardSessions(guards[0], s.Sessions)
			count, checkpoints = len(visible), 0
			for _, session := range visible {
				checkpoints += session.CheckpointsCount
			}
		}
		out["sessions"] = fmt.Sprintf("%d sessions · %d checkpoints scanned", count, checkpoints)
	}
	if s := src.Semantic; s != nil {
		out["semantic"] = fmt.Sprintf("%d symbols · %d relations · %d files", s.Symbols, s.Relations, s.Files)
	}
	if h := src.History; h != nil {
		out["history"] = fmt.Sprintf("%d records (%d decisions · %d learnings)", h.Records, h.Decisions, h.Learnings)
	}
	if f := src.Facts; f != nil {
		out["facts"] = fmt.Sprintf("%d facts across %d branch(es)", f.Facts, len(f.Branches))
	}
	if s := src.Seed; s != nil {
		out["seed"] = fmt.Sprintf("%d entrypoints · %d commands", len(s.Entrypoints), len(s.Commands))
	}
	return out
}

func dashBlindSpotLabels(spots []brainBlindSpot) []string {
	if len(spots) == 0 {
		return nil
	}
	out := make([]string, 0, len(spots))
	for _, s := range spots {
		label := s.Detail
		if label == "" {
			label = s.Code
		}
		if s.Path != "" {
			if label == "" {
				label = s.Path
			} else {
				label = s.Path + ": " + label
			}
		}
		if label == "" {
			continue
		}
		out = append(out, label)
	}
	return out
}

func dashLiveSummary(live brainLiveState) string {
	if !live.Dirty {
		return "clean"
	}
	parts := make([]string, 0, 3)
	if n := len(live.Staged); n > 0 {
		parts = append(parts, fmt.Sprintf("%d staged", n))
	}
	if n := len(live.Unstaged); n > 0 {
		parts = append(parts, fmt.Sprintf("%d unstaged", n))
	}
	if n := len(live.Untracked); n > 0 {
		parts = append(parts, fmt.Sprintf("%d untracked", n))
	}
	if len(parts) == 0 {
		return "dirty"
	}
	return strings.Join(parts, " · ")
}

func anySource(s brainStatusSources) bool {
	return s.Seed || s.Sessions || s.Semantic || s.History || s.Facts
}

func sortFreshnessAxes(axes []tui.FreshnessAxis) {
	for i := 1; i < len(axes); i++ {
		for j := i; j > 0 && axes[j-1].Name > axes[j].Name; j-- {
			axes[j-1], axes[j] = axes[j], axes[j-1]
		}
	}
}

// brainSourceReader keeps virtual transcript addresses out of the OS opener.
// Reads happen only on demand and use the same fresh privacy gate as retrieval;
// the TUI holds bytes in memory and never leaves temporary transcript files.
func brainSourceReader(ctx context.Context, runner CommandRunner, brainDir string) tui.SourceReader {
	store := newTranscriptStore(runner, brainDir)
	return func(target string) (string, bool, error) {
		rel, err := filepath.Rel(brainDir, target)
		if err != nil {
			return "", false, nil
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasPrefix(rel, "sessions/") {
			return "", false, nil
		}
		data, err := store.Read(ctx, rel)
		return string(data), true, err
	}
}
