package main

import (
	"fmt"
	"log/slog"

	"github.com/go-git/go-git/v5"
)

// repoInfo describes the commit a database was built from.
type repoInfo struct {
	Shasum  string // full hex SHA of HEAD
	Message string // HEAD's commit message
}

// repoMetadata reads the HEAD commit of the repository containing path.
//
// A repository that cannot be opened or whose HEAD cannot be read is an error:
// publishing would otherwise record provenance that is simply untrue. An
// unclean worktree only warns, because publishing a locally-modified build
// while testing is a normal thing to do.
func repoMetadata(lg *slog.Logger, path string) (*repoInfo, error) {
	repo, err := git.PlainOpenWithOptions(path, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, fmt.Errorf("opening git repo at %s: %w", path, err)
	}

	head, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("reading git HEAD: %w", err)
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, fmt.Errorf("reading commit %s: %w", head.Hash(), err)
	}

	if wt, err := repo.Worktree(); err != nil {
		lg.Warn("can't read git worktree; skipping clean check", "err", err)
	} else if st, err := wt.Status(); err != nil {
		lg.Warn("can't read git status; skipping clean check", "err", err)
	} else if !st.IsClean() {
		lg.Warn("publishing from a dirty worktree; the recorded commit does not describe the working tree",
			"shasum", head.Hash().String())
	}

	return &repoInfo{
		Shasum:  head.Hash().String(),
		Message: commit.Message,
	}, nil
}
