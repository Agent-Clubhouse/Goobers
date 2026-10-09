package childpublication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// GitTransport can inspect and create only the exact admitted reference.
type GitTransport interface {
	Head(context.Context, string, string, string) (string, error)
	Create(context.Context, string, string, string, string) error
}

// GitCommand receives host-resolved authentication. It runs transport from a
// private bare Git directory, so repository hooks, helper configuration and URL
// rewrites cannot redirect publication or expose the configured credential.
type GitCommand struct {
	Environment []string
	AllowLocal  bool
}

var commitID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Head observes the exact remote branch without consulting workspace configuration.
func (g GitCommand) Head(ctx context.Context, workspace, remote, head string) (string, error) {
	output, err := g.run(ctx, workspace, remote, "ls-remote", "--refs", remote, "refs/heads/"+head)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(output) == "" {
		return "", nil
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 1 {
		return "", errors.New("child publication ref response is ambiguous")
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 || !commitID.MatchString(fields[0]) || fields[1] != "refs/heads/"+head {
		return "", errors.New("child publication ref response differs from requested target")
	}
	return fields[0], nil
}

// Create pushes the retained commit only when the remote branch is still absent.
func (g GitCommand) Create(ctx context.Context, workspace, remote, head, sha string) error {
	if !commitID.MatchString(sha) {
		return errors.New("invalid child publication commit")
	}
	_, err := g.run(ctx, workspace, remote, "push", "--porcelain", "--force-with-lease=refs/heads/"+head+":", remote, sha+":refs/heads/"+head)
	return err
}

func (g GitCommand) run(ctx context.Context, workspace, remote string, args ...string) (string, error) {
	u, err := url.Parse(remote)
	if err != nil {
		return "", errors.New("child publication requires a trusted HTTPS remote")
	}
	localAllowed := g.AllowLocal && u.Scheme == "" && filepath.IsAbs(remote)
	if u.User != nil || (u.Scheme != "https" && !localAllowed) {
		return "", errors.New("child publication requires a trusted HTTPS remote")
	}
	objects, err := publicationGit(ctx, workspace, nil, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return "", err
	}
	objects = strings.TrimSuffix(objects, "\n")
	if !filepath.IsAbs(objects) || strings.ContainsAny(objects, "\r\n\x00") {
		return "", errors.New("child publication object custody unavailable")
	}
	format, err := publicationGit(ctx, workspace, nil, "rev-parse", "--show-object-format")
	if err != nil {
		return "", err
	}
	format = strings.TrimSpace(format)
	if format != "sha1" && format != "sha256" {
		return "", errors.New("unsupported child object format")
	}
	directory, err := os.MkdirTemp("", "goobers-child-publish-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	if _, err = publicationGit(ctx, workspace, nil, "init", "--bare", "--template=", "--object-format="+format, directory); err != nil {
		return "", err
	}
	env := g.Environment
	if env == nil {
		env = os.Environ()
	}
	clean := make([]string, 0, len(env)+3)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM":
			continue
		}
		clean = append(clean, entry)
	}
	clean = append(clean, "GIT_DIR="+directory, "GIT_OBJECT_DIRECTORY="+objects)
	return publicationGit(ctx, workspace, clean, args...)
}

func publicationGit(ctx context.Context, directory string, environment []string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"--no-replace-objects", "-c", "core.hooksPath=" + os.DevNull, "-c", "credential.helper="}, args...)...)
	command.Dir = directory
	if environment == nil {
		environment = []string{}
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(strings.ToUpper(entry), "GIT_") {
				environment = append(environment, entry)
			}
		}
	}
	command.Env = append(environment, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr publicationOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		// Remote error text can contain authenticated redirects. The durable receipt
		// records uncertainty and the stable target, never raw Git stderr or secrets.
		return "", fmt.Errorf("child publication git %s failed: %w", args[0], err)
	}
	return stdout.String(), nil
}

type publicationOutput struct{ bytes.Buffer }

func (b *publicationOutput) Write(data []byte) (int, error) {
	if b.Len()+len(data) > 16<<10 {
		return 0, errors.New("child publication git output exceeds bound")
	}
	return b.Buffer.Write(data)
}
