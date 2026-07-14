package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const usage = `git freeze [PATH]
git thaw NAME [PATH]
cryobank init ROOT
cryobank serve [--listen ADDR]
cryobank shell

Freeze uploads a repository over SSH, verifies it remotely, then moves the
local checkout to the macOS Trash. Configure the host with:

    git config --global cryobank.host HOST`

func main() {
	name := filepath.Base(os.Args[0])
	var err error
	switch name {
	case "git-freeze":
		err = freeze(os.Args[1:])
	case "git-thaw":
		err = thaw(os.Args[1:])
	default:
		err = run(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Println(usage)
		return nil
	}
	switch args[0] {
	case "init":
		return initialize(args[1:])
	case "serve":
		return serve(args[1:])
	case "shell":
		return shell(args[1:])
	case "probe", "upload", "commit":
		return remote(args)
	case "freeze":
		return freeze(args[1:])
	case "thaw":
		return thaw(args[1:])
	case "-h", "--help", "help":
		fmt.Println(usage)
		return nil
	default:
		return errors.New("unknown command; run cryobank --help")
	}
}

func freeze(args []string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println("usage: git freeze [PATH]")
		return nil
	}
	if len(args) > 1 {
		return errors.New("usage: git freeze [PATH]")
	}
	host, err := archiveTarget()
	if err != nil {
		return err
	}
	path := "."
	if len(args) == 1 {
		path = args[0]
	}
	repo, err := repository(path)
	if err != nil {
		return err
	}
	snapshot, restoreRef, err := freezeSnapshot(repo, true)
	if err != nil {
		return err
	}
	defer restoreRef()
	initialState, err := refState(repo)
	if err != nil {
		return fmt.Errorf("cannot fingerprint local refs: %w", err)
	}

	tmp, err := os.CreateTemp("", "git-cryobank-*.bundle")
	if err != nil {
		return err
	}
	bundle := tmp.Name()
	tmp.Close()
	defer os.Remove(bundle)
	if err := command(repo, "git", "bundle", "create", bundle, "--all", "HEAD"); err != nil {
		return err
	}
	digest, size, err := fileDigest(bundle)
	if err != nil {
		return err
	}
	name := filepath.Base(repo)
	if !safeName.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("repository directory name %q is unsafe; use only letters, digits, dot, underscore, and hyphen", name)
	}

	offsetText, err := ssh(host, nil, "probe", name, digest, strconv.FormatInt(size, 10))
	if err != nil {
		return err
	}
	offset, err := strconv.ParseInt(strings.TrimSpace(offsetText), 10, 64)
	if err != nil || offset < 0 || offset > size {
		return fmt.Errorf("invalid upload offset from server: %q", offsetText)
	}
	if offset < size {
		f, err := os.Open(bundle)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := ssh(host, f, "upload", name, digest, strconv.FormatInt(size, 10), strconv.FormatInt(offset, 10)); err != nil {
			return err
		}
	}
	confirmation, err := ssh(host, nil, "commit", name, digest, strconv.FormatInt(size, 10))
	if err != nil {
		return err
	}
	if strings.TrimSpace(confirmation) != "archived "+digest {
		return fmt.Errorf("remote verification failed: %s", strings.TrimSpace(confirmation))
	}
	currentSnapshot, _, err := freezeSnapshot(repo, false)
	if err != nil || currentSnapshot != snapshot {
		return errors.New("remote archive verified, but the working tree changed during upload; nothing was moved to Trash")
	}
	currentState, err := refState(repo)
	if err != nil || currentState != initialState {
		return errors.New("remote archive verified, but local refs or HEAD changed during upload; nothing was moved to Trash")
	}
	if runtime.GOOS != "darwin" {
		return errors.New("remote archive verified, but refusing removal: Trash is only supported on macOS")
	}
	if err := command("", "/usr/bin/trash", repo); err != nil {
		return fmt.Errorf("remote archive verified, but moving local repository to Trash failed: %w", err)
	}
	fmt.Printf("Froze %s to %s as %s and moved it to Trash.\n", repo, host, name)
	return nil
}

type snapshotState struct {
	Tree, Base, Branch string
}

func freezeSnapshot(repo string, writeRef bool) (snapshotState, func(), error) {
	base, err := output(repo, "git", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return snapshotState{}, func() {}, errors.New("repository has no HEAD commit")
	}
	base = strings.TrimSpace(base)
	branch, _ := output(repo, "git", "symbolic-ref", "--short", "-q", "HEAD")
	branch = strings.TrimSpace(branch)
	if branch != "" && !validBranch(repo, branch) {
		return snapshotState{}, func() {}, fmt.Errorf("branch name %q cannot be frozen", branch)
	}

	index, err := os.CreateTemp("", "git-freeze-index-*")
	if err != nil {
		return snapshotState{}, func() {}, err
	}
	indexPath := index.Name()
	index.Close()
	os.Remove(indexPath)
	defer os.Remove(indexPath)
	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	if err := commandEnv(repo, env, nil, "git", "read-tree", "HEAD"); err != nil {
		return snapshotState{}, func() {}, err
	}
	if err := commandEnv(repo, env, nil, "git", "add", "-A", "--", "."); err != nil {
		return snapshotState{}, func() {}, err
	}
	tree, err := outputEnv(repo, env, "git", "write-tree")
	state := snapshotState{strings.TrimSpace(tree), base, branch}
	if err != nil || !writeRef {
		return state, func() {}, err
	}

	ref := "refs/cryobank/freeze"
	previous, previousErr := output(repo, "git", "rev-parse", "--verify", ref)
	message := fmt.Sprintf("Cryobank freeze\n\nCryobank-Base: %s\nCryobank-Branch: %s\n", base, branch)
	commitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=Cryobank", "GIT_AUTHOR_EMAIL=cryobank@localhost",
		"GIT_COMMITTER_NAME=Cryobank", "GIT_COMMITTER_EMAIL=cryobank@localhost")
	oid, err := outputInputEnv(repo, commitEnv, strings.NewReader(message), "git", "commit-tree", state.Tree, "-p", base)
	if err != nil {
		return snapshotState{}, func() {}, err
	}
	if err := command(repo, "git", "update-ref", ref, strings.TrimSpace(oid)); err != nil {
		return snapshotState{}, func() {}, err
	}
	restore := func() {
		if previousErr == nil {
			_ = command(repo, "git", "update-ref", ref, strings.TrimSpace(previous))
		} else {
			_ = command(repo, "git", "update-ref", "-d", ref)
		}
	}
	return state, restore, nil
}

func thaw(args []string) error {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println("usage: git thaw NAME [PATH]")
		return nil
	}
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: git thaw NAME [PATH]")
	}
	name := strings.TrimSuffix(args[0], ".git")
	if !safeName.MatchString(name) || name == "." || name == ".." {
		return errors.New("invalid repository name")
	}
	host, err := archiveTarget()
	if err != nil {
		return err
	}
	dest := name
	if len(args) == 2 {
		dest = args[1]
	}
	if err := command("", "git", "clone", host+":"+name+".git", dest); err != nil {
		return err
	}
	repo, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := command(repo, "git", "fetch", "origin", "+refs/cryobank/freeze:refs/cryobank/freeze"); err != nil {
		return fmt.Errorf("clone succeeded but freeze state could not be fetched: %w", err)
	}
	if stash, _ := output(repo, "git", "ls-remote", "origin", "refs/stash"); strings.TrimSpace(stash) != "" {
		if err := command(repo, "git", "fetch", "origin", "+refs/stash:refs/stash"); err != nil {
			return fmt.Errorf("clone succeeded but stashes could not be fetched: %w", err)
		}
	}
	if err := restoreSnapshot(repo); err != nil {
		return err
	}
	if _, err := ssh(host, nil, "activate", name); err != nil {
		return fmt.Errorf("checkout restored, but Cryobank could not mark it active: %w", err)
	}
	fmt.Printf("Thawed %s into %s.\n", name, repo)
	return nil
}

func restoreSnapshot(repo string) error {
	message, err := output(repo, "git", "show", "-s", "--format=%B", "refs/cryobank/freeze")
	if err != nil {
		return err
	}
	base := trailer(message, "Cryobank-Base")
	branch := trailer(message, "Cryobank-Branch")
	if !safeOID.MatchString(base) {
		return errors.New("freeze snapshot has an invalid base commit")
	}
	if branch != "" && !validBranch(repo, branch) {
		return errors.New("freeze snapshot has an invalid branch")
	}
	if branch == "" {
		if err := command(repo, "git", "checkout", "--detach", base); err != nil {
			return err
		}
	} else if current, _ := output(repo, "git", "symbolic-ref", "--short", "-q", "HEAD"); strings.TrimSpace(current) != branch {
		if _, err := output(repo, "git", "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
			err = command(repo, "git", "checkout", branch)
		} else {
			err = command(repo, "git", "checkout", "-b", branch, "origin/"+branch)
		}
		if err != nil {
			return err
		}
	}
	patch, err := output(repo, "git", "diff", "--binary", base, "refs/cryobank/freeze")
	if err != nil {
		return err
	}
	if patch != "" {
		if err := commandEnv(repo, os.Environ(), strings.NewReader(patch), "git", "apply", "--binary"); err != nil {
			return fmt.Errorf("checkout restored but working changes could not be applied: %w", err)
		}
	}
	return nil
}

func validBranch(repo, branch string) bool {
	return exec.Command("git", "-C", repo, "check-ref-format", "--branch", branch).Run() == nil
}

func trailer(message, key string) string {
	prefix := key + ":"
	for _, line := range strings.Split(message, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func archiveTarget() (string, error) {
	target, err := output("", "git", "config", "--global", "--get", "cryobank.host")
	if err != nil {
		return "", errors.New("host is not configured; run: git config --global cryobank.host HOST")
	}
	target = strings.TrimSpace(target)
	if !validHost(target) {
		return "", errors.New("configured cryobank.host is invalid")
	}
	return target, nil
}

func repository(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	out, err := output(abs, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", errors.New("path is not inside a Git working tree")
	}
	repo := strings.TrimSpace(out)
	if filepath.Clean(abs) != filepath.Clean(repo) {
		info, statErr := os.Stat(abs)
		if statErr != nil || !info.IsDir() {
			return "", errors.New("PATH must name the repository root")
		}
	}
	return repo, nil
}

func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

func ssh(host string, stdin io.Reader, args ...string) (string, error) {
	if !validHost(host) {
		return "", errors.New("invalid SSH host")
	}
	remoteCommand := "cryobank " + strings.Join(args, " ")
	cmd := exec.Command("ssh", "--", host, remoteCommand)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ssh %s: %w: %s", host, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func validHost(host string) bool {
	return host != "" && !strings.HasPrefix(host, "-") && !strings.ContainsAny(host, " \t\r\n")
}

func readConfig(name string) (string, error) {
	path, err := configPath(name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

func writeConfig(name, value string) error {
	path, err := configPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value+"\n"), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func configPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "cryobank", name), nil
}

func command(dir, name string, args ...string) error {
	return commandEnv(dir, os.Environ(), nil, name, args...)
}

func commandEnv(dir string, env []string, stdin io.Reader, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

func output(dir, name string, args ...string) (string, error) {
	return outputEnv(dir, os.Environ(), name, args...)
}

func outputEnv(dir string, env []string, name string, args ...string) (string, error) {
	return outputInputEnv(dir, env, nil, name, args...)
}

func outputInputEnv(dir string, env []string, stdin io.Reader, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = stdin
	b, err := cmd.Output()
	return string(b), err
}
