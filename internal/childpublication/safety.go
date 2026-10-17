package childpublication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/goobers/goobers/internal/publicationguard"
)

const publicationReadLimit = 32 << 20

func (p Publisher) checkPublication(ctx context.Context, t Target, commit string) error {
	if !commitID.MatchString(commit) {
		return errors.New("child publication safety requires an exact commit")
	}
	ctx, cancel := context.WithTimeout(ctx, EffectTimeout)
	defer cancel()
	read := p.readGit
	if read == nil {
		read = publicationReadGit
	}
	git := func(ctx context.Context, args ...string) ([]byte, error) {
		return read(ctx, t.Workspace, args...)
	}
	// A parent fork can itself contain unpublished commits. Exclude only the
	// actual target base, preserving inspection of all outgoing ancestors.
	var base string
	for _, ref := range []string{"refs/remotes/origin/" + t.Base, "refs/heads/" + t.Base} {
		out, err := git(ctx, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
		if ctx.Err() != nil {
			return fmt.Errorf("child publication safety: resolve target base: %w", ctx.Err())
		}
		if err == nil && commitID.MatchString(strings.TrimSpace(string(out))) {
			base = strings.TrimSpace(string(out))
			break
		}
	}
	if base == "" {
		return errors.New("child publication safety cannot resolve target base; fetch the base branch before publishing")
	}
	checked, err := publicationguard.Check(ctx, git, base, commit)
	if err != nil {
		return err
	}
	if checked != commit {
		return errors.New("child publication safety inspected a different commit")
	}
	return nil
}

// publicationReadGit is local, bounded, and never returns raw Git diagnostics.
// Unlike transport's small response buffer, it can inspect ordinary source blobs.
func publicationReadGit(ctx context.Context, directory string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"--no-replace-objects", "-c", "core.hooksPath=" + os.DevNull, "-c", "credential.helper="}, args...)...)
	command.Dir = directory
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
	var out publicationReadBuffer
	command.Stdout = &out
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("child publication local git inspection failed: %w", err)
	}
	return out.Bytes(), nil
}

type publicationReadBuffer struct{ buf bytes.Buffer }

func (b *publicationReadBuffer) Bytes() []byte { return b.buf.Bytes() }
func (b *publicationReadBuffer) Len() int      { return b.buf.Len() }

func (b *publicationReadBuffer) Write(data []byte) (int, error) {
	if len(data) > publicationReadLimit-b.Len() {
		return 0, errors.New("child publication inspection exceeds size limit")
	}
	return b.buf.Write(data)
}
