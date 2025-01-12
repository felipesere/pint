package discovery

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"

	"golang.org/x/exp/slices"

	"github.com/cloudflare/pint/internal/git"
	"github.com/cloudflare/pint/internal/output"
	"github.com/cloudflare/pint/internal/parser"
)

func NewGitBranchFinder(
	gitCmd git.CommandRunner,
	filter git.PathFilter,
	baseBranch string,
	maxCommits int,
) GitBranchFinder {
	return GitBranchFinder{
		gitCmd:     gitCmd,
		filter:     filter,
		baseBranch: baseBranch,
		maxCommits: maxCommits,
	}
}

type GitBranchFinder struct {
	gitCmd     git.CommandRunner
	baseBranch string
	filter     git.PathFilter
	maxCommits int
}

func (f GitBranchFinder) Find(allEntries []Entry) (entries []Entry, err error) {
	for i := range allEntries {
		allEntries[i].State = Excluded
	}

	cr, err := git.CommitRange(f.gitCmd, f.baseBranch)
	if err != nil {
		return nil, fmt.Errorf("failed to get the list of commits to scan: %w", err)
	}
	slog.Debug("Got commit range from git", slog.String("from", cr.From), slog.String("to", cr.To))

	if len(cr.Commits) > f.maxCommits {
		return nil, fmt.Errorf("number of commits to check (%d) is higher than maxCommits (%d), exiting", len(cr.Commits), f.maxCommits)
	}

	changes, err := git.Changes(f.gitCmd, cr, f.filter)
	if err != nil {
		return nil, err
	}

	shouldSkip, err := f.shouldSkipAllChecks(changes)
	if err != nil {
		return nil, err
	}
	if shouldSkip {
		return nil, nil
	}

	for _, change := range changes {
		var entriesBefore, entriesAfter []Entry
		entriesBefore, _ = readRules(
			change.Path.Before.EffectivePath(),
			change.Path.Before.Name,
			bytes.NewReader(change.Body.Before),
			!f.filter.IsRelaxed(change.Path.Before.Name),
		)
		entriesAfter, err = readRules(
			change.Path.After.EffectivePath(),
			change.Path.After.Name,
			bytes.NewReader(change.Body.After),
			!f.filter.IsRelaxed(change.Path.After.Name),
		)
		if err != nil {
			return nil, fmt.Errorf("invalid file syntax: %w", err)
		}

		for _, me := range matchEntries(entriesBefore, entriesAfter) {
			switch {
			case !me.hasBefore && me.hasAfter:
				me.after.State = Added
				me.after.ModifiedLines = commonLines(change.Body.ModifiedLines, me.after.ModifiedLines)
				slog.Debug(
					"Rule added on HEAD branch",
					slog.String("name", me.after.Rule.Name()),
					slog.String("state", me.after.State.String()),
					slog.String("path", me.after.SourcePath),
					slog.String("ruleLines", me.after.Rule.Lines.String()),
					slog.String("modifiedLines", output.FormatLineRangeString(me.after.ModifiedLines)),
				)
				entries = append(entries, me.after)
			case me.hasBefore && me.hasAfter:
				switch {
				case me.isIdentical && !me.wasMoved:
					slog.Debug(
						"Rule content was not modified on HEAD, identical rule present before",
						slog.String("name", me.after.Rule.Name()),
						slog.String("lines", me.after.Rule.Lines.String()),
					)
					me.after.State = Excluded
					me.after.ModifiedLines = []int{}
				case me.wasMoved:
					slog.Debug(
						"Rule content was not modified on HEAD but the file was moved or renamed",
						slog.String("name", me.after.Rule.Name()),
						slog.String("lines", me.after.Rule.Lines.String()),
					)
					me.after.State = Moved
					me.after.ModifiedLines = git.CountLines(change.Body.After)
				default:
					slog.Debug(
						"Rule modified on HEAD branch",
						slog.String("name", me.after.Rule.Name()),
						slog.String("state", me.after.State.String()),
						slog.String("path", me.after.SourcePath),
						slog.String("ruleLines", me.after.Rule.Lines.String()),
						slog.String("modifiedLines", output.FormatLineRangeString(me.after.ModifiedLines)),
					)
					me.after.State = Modified
					me.after.ModifiedLines = commonLines(change.Body.ModifiedLines, me.after.ModifiedLines)
				}
				entries = append(entries, me.after)
			case me.hasBefore && !me.hasAfter:
				me.before.State = Removed
				ml := commonLines(change.Body.ModifiedLines, me.before.ModifiedLines)
				if len(ml) > 0 {
					me.before.ModifiedLines = ml
				}
				slog.Debug(
					"Rule removed on HEAD branch",
					slog.String("name", me.before.Rule.Name()),
					slog.String("state", me.before.State.String()),
					slog.String("path", me.before.SourcePath),
					slog.String("ruleLines", me.before.Rule.Lines.String()),
					slog.String("modifiedLines", output.FormatLineRangeString(me.before.ModifiedLines)),
				)
				entries = append(entries, me.before)
			default:
				slog.Debug(
					"Unknown rule",
					slog.String("state", me.before.State.String()),
					slog.String("path", me.before.SourcePath),
					slog.String("modifiedLines", output.FormatLineRangeString(me.before.ModifiedLines)),
				)
				entries = append(entries, me.after)
			}
		}
	}

	symlinks, err := addSymlinkedEntries(entries)
	if err != nil {
		return nil, err
	}

	for _, entry := range symlinks {
		if f.filter.IsPathAllowed(entry.SourcePath) {
			entries = append(entries, entry)
		}
	}

	var found bool
	for _, entry := range entries {
		found = false
		if entry.State == Removed {
			goto NEXT
		}
		for i, globEntry := range allEntries {
			if entry.SourcePath == globEntry.SourcePath && entry.Rule.IsSame(globEntry.Rule) {
				allEntries[i].State = entry.State
				allEntries[i].ModifiedLines = entry.ModifiedLines
				found = true
				break
			}
		}
	NEXT:
		if !found {
			allEntries = append(allEntries, entry)
		}
	}

	slog.Debug("Git branch finder completed", slog.Int("count", len(allEntries)))
	return allEntries, nil
}

func (f GitBranchFinder) shouldSkipAllChecks(changes []*git.FileChange) (bool, error) {
	commits := map[string]struct{}{}
	for _, change := range changes {
		for _, commit := range change.Commits {
			commits[commit] = struct{}{}
		}
	}

	for commit := range commits {
		msg, err := git.CommitMessage(f.gitCmd, commit)
		if err != nil {
			return false, fmt.Errorf("failed to get commit message for %s: %w", commit, err)
		}
		for _, comment := range []string{"[skip ci]", "[no ci]"} {
			if strings.Contains(msg, comment) {
				slog.Info(
					fmt.Sprintf("Found a commit with '%s', skipping all checks", comment),
					slog.String("commit", commit))
				return true, nil
			}
		}
	}

	return false, nil
}

func commonLines(a, b []int) (common []int) {
	for _, ai := range a {
		if slices.Contains(b, ai) {
			common = append(common, ai)
		}
	}
	for _, bi := range b {
		if slices.Contains(a, bi) && !slices.Contains(common, bi) {
			common = append(common, bi)
		}
	}
	return common
}

type matchedEntry struct {
	before      Entry
	after       Entry
	hasBefore   bool
	hasAfter    bool
	isIdentical bool
	wasMoved    bool
}

func matchEntries(before, after []Entry) (ml []matchedEntry) {
	for _, a := range after {
		m := matchedEntry{after: a, hasAfter: true}
		beforeSwap := make([]Entry, 0, len(before))
		var matches []Entry
		var matched bool

		for _, b := range before {
			if !matched && a.Rule.Name() != "" && a.Rule.IsIdentical(b.Rule) {
				m.before = b
				m.hasBefore = true
				m.isIdentical = true
				m.wasMoved = a.SourcePath != b.SourcePath
				matched = true
			} else {
				beforeSwap = append(beforeSwap, b)
			}
		}
		before = beforeSwap

		if !matched {
			before, matches = findRulesByName(before, a.Rule.Name(), a.Rule.Type())
			switch len(matches) {
			case 0:
			case 1:
				m.before = matches[0]
				m.hasBefore = true
			default:
				before = append(before, matches...)
			}
		}

		ml = append(ml, m)
	}

	for _, b := range before {
		b := b
		ml = append(ml, matchedEntry{before: b, hasBefore: true})
	}

	return ml
}

func findRulesByName(entries []Entry, name string, typ parser.RuleType) (nomatch, match []Entry) {
	for _, entry := range entries {
		if entry.PathError == nil && entry.Rule.Type() == typ && entry.Rule.Name() == name {
			match = append(match, entry)
		} else {
			nomatch = append(nomatch, entry)
		}
	}
	return nomatch, match
}
